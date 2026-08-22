package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Механизм «у тебя старая версия». Клиент на старте спрашивает GET /version и
// получает вердикт: всё в порядке, стоит обновиться или обновиться обязательно.
// Нужен он не сам по себе — а чтобы знать, когда безопасно снимать способы
// работы, которые поддерживают только старые сборки (см. ether-meta/PLANS.md).
//
// Версия НЕ ездит с каждым запросом заголовком: это разовый вопрос на старте, и
// таскать её всюду означало бы трогать весь клиентский транспорт ради одного
// ответа. По той же причине здесь нет и серверной страховки — отказа старым
// сборкам на изменяющих эндпоинтах: чтобы её сделать, версию пришлось бы
// приносить с запросами. Вердикт уважает клиент.

// softDelay — сколько ждём после выхода версии, прежде чем предлагать
// обновление. «Версия вышла» ≠ «версия доступна всем»: и Apple (phased release),
// и Google (staged rollout) раскатывают сборку долями пользователей несколько
// дней, а сразу после загрузки она может ещё висеть в обработке или ревью.
// Предлагать обновиться тому, кто физически не может обновиться, — хуже, чем
// промолчать.
//
// Константа, а не поле конфига: нужен другой срок для конкретного релиза —
// сдвигается latest_since, а не заводится ещё одна настройка.
const softDelay = 7 * 24 * time.Hour

// dateLayout — формат latest_since в конфиге. Дату разбираем сами, а не отдаём
// её YAML-типу timestamp: тогда опечатка — понятная ошибка старта с указанием
// платформы, а не молча нулевое время.
const dateLayout = "2006-01-02"

// Значения UpdateData.Status.
const (
	updateOK       = "ok"       // сборка свежая — или новее latest, как у dev-сборок
	updateSoft     = "soft"     // есть версия свежее: предложить, но не настаивать
	updateRequired = "required" // ниже min: сборка объявлена неработающей
)

// ClientVersionRule — блок одной платформы в client_versions (см. Config). Все
// три поля ОБЯЗАТЕЛЬНЫ: половина правила — это не «часть механизма работает», а
// молчаливо другое поведение, чем автор конфига имел в виду. Забыть блок целиком
// можно (тогда про эту платформу мы просто ничего не говорим), забыть поле — нет.
//
// Значения строками, а не числами и датами: разбираем их сами (newVersionGate),
// чтобы опечатка была ошибкой старта с внятным текстом.
type ClientVersionRule struct {
	Min         string `yaml:"min"`
	Latest      string `yaml:"latest"`
	LatestSince string `yaml:"latest_since"`
}

// versionGate — разобранные и проверенные пороги из конфига. Пустой (или nil)
// gate всегда отвечает ok: конфиг без client_versions — рабочая ситуация,
// сервер поднимается и молчит про обновления.
type versionGate struct {
	rules map[string]versionRule
}

type versionRule struct {
	min, latest semver
	latestRaw   string
	softFrom    time.Time // с какого момента отдаём soft: latest_since + softDelay
	url         string
}

// newVersionGate разбирает client_versions. Любое пропущенное или невалидное
// значение — ошибка старта: неверный порог хуже отсутствующего, потому что тихо
// блокирует людей или тихо молчит, и заметить это можно только по жалобам.
func newVersionGate(rules map[string]ClientVersionRule) (*versionGate, error) {
	g := &versionGate{rules: make(map[string]versionRule, len(rules))}
	for platform, r := range rules {
		if platform != platformIOS && platform != platformAndroid {
			return nil, fmt.Errorf("client_versions: неизвестная платформа %q (ожидается %s или %s)",
				platform, platformIOS, platformAndroid)
		}
		out := versionRule{latestRaw: r.Latest, url: storeURL(platform)}
		var err error
		if out.min, err = parseVersionField(platform, "min", r.Min); err != nil {
			return nil, err
		}
		if out.latest, err = parseVersionField(platform, "latest", r.Latest); err != nil {
			return nil, err
		}
		if r.LatestSince == "" {
			return nil, fmt.Errorf("client_versions.%s.latest_since: не задано (ожидается дата вида 2026-08-22)",
				platform)
		}
		since, err := time.Parse(dateLayout, r.LatestSince)
		if err != nil {
			return nil, fmt.Errorf("client_versions.%s.latest_since: %q — ожидается дата вида 2026-08-22",
				platform, r.LatestSince)
		}
		out.softFrom = since.Add(softDelay)
		// min выше latest заблокировал бы всех, включая тех, у кого стоит самая
		// свежая сборка: обновляться им некуда. Это всегда опечатка.
		if out.latest.less(out.min) {
			return nil, fmt.Errorf("client_versions.%s: min (%s) выше latest (%s) — заблокировало бы всех",
				platform, r.Min, r.Latest)
		}
		g.rules[platform] = out
	}
	return g, nil
}

// verdict — что ответить клиенту. Платформы нет в конфиге, версия не разобралась
// или сборка НОВЕЕ latest (dev и TestFlight идут впереди) → ok: гадать нельзя, а
// подгонять человека к версии старше его собственной — тем более.
func (g *versionGate) verdict(platform, clientVersion string, now time.Time) UpdateData {
	if g == nil {
		return UpdateData{Status: updateOK}
	}
	rule, ok := g.rules[platform]
	if !ok {
		return UpdateData{Status: updateOK}
	}
	out := UpdateData{Status: updateOK, Latest: rule.latestRaw}
	v, ok := parseSemver(clientVersion)
	if !ok {
		return out
	}
	switch {
	case v.less(rule.min):
		out.Status = updateRequired
	case v.less(rule.latest) && !now.Before(rule.softFrom):
		out.Status = updateSoft
	}
	if out.Status != updateOK {
		out.URL = rule.url
	}
	return out
}

// storeURL — куда ведёт кнопка «Обновить».
//
// Прямо в стор, а НЕ на https://etherapp.ru/app?src=update, как предполагал
// план: домен etherapp.ru заявлен в applinks (ios/Runner/Runner.entitlements),
// поэтому на iOS такая ссылка откроет не Safari с редиректом в App Store, а сам
// Эфир — кнопка «Обновить» вернула бы человека туда, откуда он её нажал. Счётчик
// нажатий мы при этом теряем, но распределение версий и так есть (client_version).
func storeURL(platform string) string {
	switch platform {
	case platformIOS:
		return appStoreURL
	case platformAndroid:
		return playURL
	}
	return ""
}

func parseVersionField(platform, field, raw string) (semver, error) {
	if raw == "" {
		return semver{}, fmt.Errorf("client_versions.%s.%s: не задано (ожидается версия вида 1.2.0)",
			platform, field)
	}
	v, ok := parseSemver(raw)
	if !ok {
		return semver{}, fmt.Errorf("client_versions.%s.%s: %q — ожидается версия вида 1.2.0",
			platform, field, raw)
	}
	return v, nil
}

// semver — версия приложения как тройка: «1.2.0» из pubspec.
type semver struct{ major, minor, patch int }

// parseSemver разбирает «1.2.0». Ровно три числа: pubspec у нас такой всегда, а
// прощать «1.2» значит гадать, что имел в виду тот, кто это прислал.
func parseSemver(s string) (semver, bool) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, dst := range []*int{&v.major, &v.minor, &v.patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return semver{}, false
		}
		*dst = n
	}
	return v, true
}

func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}
