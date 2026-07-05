package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
// номер дома, код страны). Только такие записи годятся в кандидаты слотов.
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
	if e.Class == "place" && e.Type == "quarter" && numericName.MatchString(e.LocalName) {
		return false
	}
	return true
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
		return fmt.Errorf("nominatim: HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("nominatim: bad JSON: %w", err)
	}
	if raw := out.errRaw(); len(raw) > 0 && string(raw) != "null" {
		return fmt.Errorf("nominatim: %s", raw)
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
		return nil, fmt.Errorf("lat must be in [-90, 90], got %v", lat)
	}
	if lng < -180 || lng > 180 {
		return nil, fmt.Errorf("lng must be in [-180, 180], got %v", lng)
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

	// Пустые слоты опускаются (контракт Geocoder).
	out := make([]Channel, 0, 5)
	add := func(level, label, isoID string, cand *nomAddressEntry) {
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
		out = append(out, Channel{ID: id, Level: level, Label: label, Name: cand.LocalName})
	}
	add("country", "Страна", countryISO, countryEntry)
	add("region", "Область", regionISO, regionEntry)
	add("city", "Город", "", cityEntry)
	add("district", "Район", "", districtEntry)
	add("quarter", "Квартал", "", quarterEntry)
	return out, nil
}
