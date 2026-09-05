package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Причины отказа — метки для статистики (geocode_request.error). Различать их
// надо не из любви к деталям: HTTP 429 означает «нас притормозили за превышение
// лимита» и лечится своим инстансом, таймаут — «Nominatim медленный», а 400 —
// «мы спросили ерунду», и это правится у нас в коде. В одном счётчике «отказы»
// эти три случая требуют совершенно разных решений и потому бесполезны.
const (
	geocodeErrTimeout   = "timeout"
	geocodeErrNetwork   = "network"
	geocodeErrBadJSON   = "bad_json"
	geocodeErrNominatim = "nominatim_error"
	geocodeErrBadCoords = "bad_coords"
	geocodeErrOther     = "other"
)

// Сентинелы: по ним причина определяется через errors.Is, а не разбором текста
// ошибки — текст можно поправить, не сломав статистику.
var (
	errNomBadJSON = errors.New("nominatim: тело не разобралось")
	errNomPayload = errors.New("nominatim: ошибка в ответе")
	errBadCoords  = errors.New("координаты вне диапазона")
)

// nomHTTPError — ответ не 200. Код хранится целиком: 429 и 400 значат разное.
type nomHTTPError struct{ Status int }

func (e nomHTTPError) Error() string { return fmt.Sprintf("nominatim: HTTP %d", e.Status) }

// geocodeReason — метка причины для статистики; «» если ошибки не было.
func geocodeErrLabel(err error) string {
	if err == nil {
		return ""
	}
	var httpErr nomHTTPError
	if errors.As(err, &httpErr) {
		return fmt.Sprintf("http_%d", httpErr.Status)
	}
	// Таймаут проверяем ДО сетевой ошибки: таймаут http.Client приходит тем же
	// *url.Error, и общая ветка проглотила бы его первой.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return geocodeErrTimeout
	}
	switch {
	case errors.Is(err, errNomBadJSON):
		return geocodeErrBadJSON
	case errors.Is(err, errNomPayload):
		return geocodeErrNominatim
	case errors.Is(err, errBadCoords):
		return geocodeErrBadCoords
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return geocodeErrNetwork
	}
	return geocodeErrOther
}

// NominatimGeocoder — порт логики из ether-research/nominatim_hierarchy.js.
// Подход: 1 reverse (находит самую локальную точку) + 1 /details (отдаёт всю
// цепочку родителей с osm_id и нормализованным rank_address). Из цепочки слоты
// выбираются по диапазонам rank_address, ID — по контракту ether-meta
// (ISO 3166-1/-2 для страны/области, osm_type/osm_id для остальных).
//
// ВНИМАНИЕ: публичный nominatim.openstreetmap.org — лимит 1 req/s и не для
// production. Все запросы сериализуются мьютексом с паузой minInterval, поэтому
// один вызов Channels занимает ~1–3 секунды.
type NominatimGeocoder struct {
	BaseURL   string
	UserAgent string

	// OnUnmapped — куда сообщать о единице, которой словарь подписей не знает
	// (см. unit_title.go). Хук, а не Store полем: геокодер про хранение ничего
	// не знает и знать не должен, а в тестах и в стабе писать некуда. nil — не
	// сообщаем.
	//
	// Зовётся только на ПРОМАХЕ геокеша (клетка ~100 м, сутки), потому что при
	// попадании геокодер не работает вовсе: счётчик в таблице — это число
	// промахов, а не людей, увидевших подпись.
	OnUnmapped func(gap unitGap, slot, country string)

	client      *http.Client
	minInterval time.Duration

	mu   sync.Mutex // сериализует запросы ради лимита публичного сервера
	last time.Time
}

func NewNominatimGeocoder() *NominatimGeocoder {
	return &NominatimGeocoder{
		BaseURL:     "https://nominatim.openstreetmap.org",
		UserAgent:   "ether-server/0.1 (geo-chat prototype)",
		client:      &http.Client{Timeout: 15 * time.Second},
		minInterval: 1100 * time.Millisecond,
	}
}

// ─── Модель слотов ────────────────────────────────────────────────────────────

// Верхние слоты — по диапазонам rank_address: в каждом берём САМУЮ СПЕЦИФИЧНУЮ
// единицу (макс. rank), поэтому промежуточные тиры (федеральный округ в РФ,
// county в США) отваливаются сами.
var upperSlots = []struct {
	level, label     string
	rankMin, rankMax int
}{
	{"country", "Страна", 1, 4},
	{"region", "Область", 5, 9},
	{"city", "Город", 10, 16},
}

// Район + Квартал — НЕ по фиксированным рангам (один ранг означает разное в
// разных странах: rank 20 — «район» в РФ, но «квартал»-Stadtteil в ФРГ). Берём
// две самые специфичные подгородские единицы из rank 17–25: мельчайшая →
// Квартал, следующая по охвату → Район.
const (
	subcityRankMin = 17
	subcityRankMax = 25
)

// ─── Ответы Nominatim ─────────────────────────────────────────────────────────

var osmTypeFull = map[string]string{"N": "node", "W": "way", "R": "relation"}
var osmTypeChar = map[string]string{"node": "N", "way": "W", "relation": "R"}

type nomReverse struct {
	Error   json.RawMessage   `json:"error"`
	OSMType string            `json:"osm_type"` // "node" | "way" | "relation"
	OSMID   int64             `json:"osm_id"`
	Address map[string]string `json:"address"`
}

type nomDetails struct {
	Error   json.RawMessage   `json:"error"`
	Address []nomAddressEntry `json:"address"`
}

type nomAddressEntry struct {
	LocalName   string `json:"localname"`
	OSMType     string `json:"osm_type"` // "N" | "W" | "R"
	OSMID       int64  `json:"osm_id"`
	Class       string `json:"class"`
	Type        string `json:"type"`
	AdminLevel  int    `json:"admin_level"`
	RankAddress int    `json:"rank_address"`
	IsAddress   *bool  `json:"isaddress"` // отсутствие поля трактуем как true
}

// ref — стабильная ссылка OSM: "relation/2555133" (или "" если её нет).
func (e *nomAddressEntry) ref() string {
	if e == nil || e.OSMType == "" || e.OSMID == 0 {
		return ""
	}
	t, ok := osmTypeFull[e.OSMType]
	if !ok {
		t = e.OSMType
	}
	return fmt.Sprintf("%s/%d", t, e.OSMID)
}

var numericName = regexp.MustCompile(`^\d+$`)

// isUnit — реальная территориальная единица, а не адресный скаляр (индекс,
// номер дома, код страны) и не посторонний объект, случайно попавший в цепочку.
// Только такие записи годятся в кандидаты слотов.
//
// Канал — это **административная граница либо населённое место**, поэтому
// кандидатом может быть только `boundary/administrative` или `place/*`. Правило
// сформулировано положительно, а не списком запретов, потому что запреты
// приходилось бы дописывать вечно: в цепочке лежит всё, у чего подошёл ранг.
// Что оно отсекает — не гипотеза, а наблюдения на сверенных локациях:
//
//   - `highway/pedestrian` «Красная площадь» — пешеходная зона. Она вытесняла
//     Тверской район ИЗ НАБОРА: слотов под городом два, площадь заняла один и
//     сдвинула Китай-город из Квартала в Район;
//   - `landuse/commercial` «Times Square» и «Golden Shoe» — то же самое в
//     Нью-Йорке и Сингапуре, землепользование вместо квартала;
//   - `boundary/historic` «Китай-город» — историческое название, а не единица:
//     границы юрисдикции за ним нет, а лежит оно внутри Тверского района.
//
// Ниже квартала мы и так не спускаемся (улица — линия, дом и POI слишком
// дробны); площадь и торговый квартал — ровно та же категория, просто нарисованы
// полигоном и потому пролезали по рангу.
//
// Отдельно отсекаем place/quarter с чисто числовым именем ("4", "14", ...) —
// это узлы внутренней адресной сетки OSM (наблюдалось в Москве: несколько таких
// узлов на одном ранге рядом с настоящим районом), а не именованная единица.
// Без этого фильтра сетка выигрывала у административного района как более
// специфичная по рангу и подменяла собой слот «Район» (порт фикса из
// ether-research/nominatim_hierarchy.js).
func (e *nomAddressEntry) isUnit() bool {
	if e.IsAddress != nil && !*e.IsAddress {
		return false
	}
	if e.Type == "postcode" || e.Type == "house_number" {
		return false
	}
	if e.Class != "place" && !(e.Class == "boundary" && e.Type == "administrative") {
		return false
	}
	if e.Class == "place" && e.Type == "quarter" && numericName.MatchString(e.LocalName) {
		return false
	}
	return true
}

// sameUnitName — две единицы названы одинаково (без учёта регистра и краевых
// пробелов). Пустое имя совпадением не считается: у безымянных записей нечего
// сравнивать, а «оба пустые» дало бы ложное срабатывание правила фед. города.
func sameUnitName(a, b *nomAddressEntry) bool {
	if a == nil || b == nil {
		return false
	}
	an, bn := strings.TrimSpace(a.LocalName), strings.TrimSpace(b.LocalName)
	return an != "" && strings.EqualFold(an, bn)
}

// ─── HTTP ─────────────────────────────────────────────────────────────────────

// getJSON выполняет запрос под мьютексом: публичный Nominatim требует не чаще
// 1 req/s, поэтому между любыми двумя запросами выдерживается minInterval.
func (g *NominatimGeocoder) getJSON(rawURL string, out interface{ errRaw() json.RawMessage }) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if wait := g.minInterval - time.Since(g.last); wait > 0 {
		time.Sleep(wait)
	}
	defer func() { g.last = time.Now() }()

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", g.UserAgent)

	res, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nomHTTPError{res.StatusCode}
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %v", errNomBadJSON, err)
	}
	if raw := out.errRaw(); len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("%w: %s", errNomPayload, raw)
	}
	return nil
}

func (r *nomReverse) errRaw() json.RawMessage { return r.Error }
func (d *nomDetails) errRaw() json.RawMessage { return d.Error }

func (g *NominatimGeocoder) reverse(lat, lng float64) (*nomReverse, error) {
	q := url.Values{}
	q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
	q.Set("lon", strconv.FormatFloat(lng, 'f', -1, 64))
	q.Set("zoom", "18")
	q.Set("format", "jsonv2")
	q.Set("addressdetails", "1")
	var out nomReverse
	if err := g.getJSON(g.BaseURL+"/reverse?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (g *NominatimGeocoder) details(osmTypeFullName string, osmID int64) ([]nomAddressEntry, error) {
	tc, ok := osmTypeChar[osmTypeFullName]
	if !ok {
		return nil, fmt.Errorf("nominatim: unknown osm_type %q", osmTypeFullName)
	}
	q := url.Values{}
	q.Set("osmtype", tc)
	q.Set("osmid", strconv.FormatInt(osmID, 10))
	q.Set("addressdetails", "1")
	q.Set("format", "json")
	var out nomDetails
	if err := g.getJSON(g.BaseURL+"/details?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.Address, nil
}

// ─── Координаты → каналы ──────────────────────────────────────────────────────

var isoRegionKey = regexp.MustCompile(`^ISO3166-2-lvl(\d+)$`)

func (g *NominatimGeocoder) Channels(lat, lng float64) ([]Channel, error) {
	if lat < -90 || lat > 90 {
		return nil, fmt.Errorf("%w: lat=%v, must be in [-90, 90]", errBadCoords, lat)
	}
	if lng < -180 || lng > 180 {
		return nil, fmt.Errorf("%w: lng=%v, must be in [-180, 180]", errBadCoords, lng)
	}

	rev, err := g.reverse(lat, lng)
	if err != nil {
		return nil, err
	}
	levels, err := g.details(rev.OSMType, rev.OSMID)
	if err != nil {
		return nil, err
	}
	addr := rev.Address

	// Вся цепочка от общего к частному; стабильная сортировка — детерминизм
	// набора ID при равных рангах обязателен по контракту.
	sort.SliceStable(levels, func(i, j int) bool {
		return levels[i].RankAddress < levels[j].RankAddress
	})

	// Самая специфичная единица в диапазоне ранга (при равных рангах — первая
	// в порядке цепочки).
	pickBand := func(min, max int, exclude *nomAddressEntry) *nomAddressEntry {
		var best *nomAddressEntry
		for i := range levels {
			l := &levels[i]
			if !l.isUnit() || l.RankAddress < min || l.RankAddress > max {
				continue
			}
			if exclude.ref() != "" && l.ref() == exclude.ref() {
				continue
			}
			if best == nil || l.RankAddress > best.RankAddress {
				best = l
			}
		}
		return best
	}

	countryEntry := pickBand(upperSlots[0].rankMin, upperSlots[0].rankMax, nil)
	countryISO := strings.ToUpper(addr["country_code"])

	// Область привязываем к admin_level, на котором задан ISO 3166-2 (поле
	// ISO3166-2-lvlN само называет уровень). Именно эта единица — субъект ISO,
	// независимо от ранга: в Японии 都 (adm 4) лежит на ранге города (16).
	// Если ключей несколько (Франция: lvl4 регион + lvl6 департамент), берём
	// наименьший N — самый широкий уровень, это и есть слот «Область»; выбор
	// по map-итерации был бы недетерминирован.
	regionEntry := pickBand(upperSlots[1].rankMin, upperSlots[1].rankMax, nil)
	regionISO, isoLevel := "", 0
	for k, v := range addr {
		m := isoRegionKey.FindStringSubmatch(k)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if isoLevel == 0 || n < isoLevel {
			isoLevel, regionISO = n, v
		}
	}
	if isoLevel != 0 {
		for i := range levels {
			l := &levels[i]
			if l.isUnit() && l.AdminLevel == isoLevel {
				regionEntry = l
				break
			}
		}
	}

	// Город — без единицы, ушедшей в Область (иначе дубль, когда префектура = город).
	cityEntry := pickBand(upperSlots[2].rankMin, upperSlots[2].rankMax, regionEntry)

	// Город федерального значения (Москва, СПб, Севастополь, Mexico City): субъект
	// ISO там и есть сам город, но в OSM это РАЗНЫЕ объекты (adm-4 граница + ярлык
	// place/city), поэтому исключение выше их не ловит и слот Области дублирует
	// Город. Информации Область при этом не несёт: у жителя такого города она всегда
	// одна и та же и не связана с одноимённой областью вокруг (Москва не входит в
	// Московскую область) — гасим слот.
	//
	// Признак — то же имя И то, что Город это place/*-ярлык, а не своя админединица.
	// Проверка class обязательна: у Нью-Йорка штат и город тоже совпадают по имени,
	// но город там boundary/administrative (adm 5), т.е. реальная единица ВНУТРИ
	// штата, и US-NY терять нельзя.
	if sameUnitName(regionEntry, cityEntry) && cityEntry.Class != "boundary" {
		regionEntry = nil // без отката на pickBand(5,9): там лежит лишний тир (фед. округ)
	}

	// Район + Квартал: две самые специфичные подгородские единицы.
	var sub []*nomAddressEntry
	for i := range levels {
		l := &levels[i]
		if l.isUnit() && l.RankAddress >= subcityRankMin && l.RankAddress <= subcityRankMax {
			sub = append(sub, l)
		}
	}
	sort.SliceStable(sub, func(i, j int) bool {
		return sub[i].RankAddress > sub[j].RankAddress
	})
	var districtEntry, quarterEntry *nomAddressEntry
	switch {
	case len(sub) >= 2:
		quarterEntry, districtEntry = sub[0], sub[1] // мельчайшая → Квартал
	case len(sub) == 1:
		districtEntry = sub[0] // единственная подгородская — Район (пол гарантии)
	}

	// Пустые слоты опускаются (контракт Geocoder). Планета идёт первой: она есть
	// у всех и из координат не выводится (см. PlanetChannel).
	out := make([]Channel, 0, 6)
	out = append(out, PlanetChannel)
	add := func(level, slotLabel, isoID string, cand *nomAddressEntry) {
		if cand == nil {
			return
		}
		id := isoID
		if id == "" {
			id = cand.ref()
		}
		if id == "" {
			return
		}
		// Подпись — про тип единицы, а не про слот: в «Город» под Истрой
		// ложится муниципальный округ, а в «Район» — деревня (см. unit_title.go).
		label, name, gap := unitTitle(cand, slotLabel)
		if gap != nil && g.OnUnmapped != nil {
			g.OnUnmapped(*gap, level, countryISO)
		}
		out = append(out, Channel{ID: id, Level: level, Label: label, Name: name})
	}
	add("country", "Страна", countryISO, countryEntry)
	add("region", "Область", regionISO, regionEntry)
	add("city", "Город", "", cityEntry)
	add("district", "Район", "", districtEntry)
	add("quarter", "Квартал", "", quarterEntry)
	return out, nil
}
