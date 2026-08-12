package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// REST — синхронный запрос-ответ без побочных эффектов на живом WS-соединении:
// вход через провайдера (auth), resume, logout, удаление аккаунта,
// accept_rules, жалобы, history. На WebSocket остались только locate и
// publish/message — см. client.go и ether-meta/PROTOCOL.md.
//
// Идентификация здесь полностью стейтлесс: "аутентифицирован" значит "прислал
// валидный токен сессии в этом запросе", без привязки к какому-либо Client —
// в отличие от WS, где authed живёт на структуре соединения.
//
// Порядок в файле: маршруты и хендлеры идут **по алфавиту пути** — так у нового
// эндпоинта есть единственное очевидное место, а не «в конец». Общие хелперы
// ответа — в конце файла.

// notify может быть nil — служебные уведомления в Telegram выключены (см.
// notify.go); на приём жалоб и регистрацию это не влияет.
// verifiers — проверяльщики ID-token по провайдерам (ключ — ProviderTelegram и
// т.д.). Эндпоинты регистрируются на ВСЕ известные провайдеры, даже незаданные:
// незаданный отвечает `501 provider_disabled` с обычным JSON-телом. Раньше он
// просто не регистрировался, и клиент получал текстовый `404 page not found` от
// net/http — на нём падал разбор JSON, и любая настроечная ошибка выглядела в
// приложении как невнятное «Не удалось войти».
func registerREST(mux *http.ServeMux, store *Store, verifiers map[string]*Verifier, notify *Notifier) {
	mux.HandleFunc("/account/delete", handleDeleteAccount(store))
	mux.HandleFunc("/app", handleAppLink(store))
	for _, provider := range []string{ProviderApple, ProviderGoogle, ProviderTelegram} {
		mux.HandleFunc("/auth/"+provider, handleAuth(store, provider, verifiers[provider], notify))
	}
	mux.HandleFunc("/health", handleHealth(store))
	mux.HandleFunc("/history", handleHistory(store))
	mux.HandleFunc("/block", handleBlock(store, notify))
	mux.HandleFunc("/blocked", handleBlocked(store))
	mux.HandleFunc("/profile/name", handleSetName(store))
	for _, provider := range []string{ProviderApple, ProviderGoogle, ProviderTelegram} {
		mux.HandleFunc("/profile/link/"+provider,
			handleLink(store, provider, verifiers[provider]))
	}
	mux.HandleFunc("/push/register", handlePushRegister(store))
	mux.HandleFunc("/push/unregister", handlePushUnregister(store))
	mux.HandleFunc("/report", handleReport(store, notify))
	mux.HandleFunc("/rules/accept", handleAcceptRules(store))
	mux.HandleFunc("/session/logout", handleLogout(store))
	mux.HandleFunc("/session/resume", handleResume(store))
}

// handleBlock — POST /block {token, user_id, unblock?} → 200 {} | 401 bad_session
// | 400 bad_data — заблокировать (или разблокировать) другого пользователя.
//
// Требование Apple 1.2: «mechanism for users to block abusive users», причём
// блокировка обязана убрать контент из ленты немедленно и уведомить
// разработчика. Поэтому здесь три эффекта: запись в blocks (историю сервер
// фильтрует сразу, живую ленту — клиент), исключение из пушей (см. PushTargets)
// и пост в служебный канал модерации.
//
// Блокировка односторонняя и модерацию не заменяет: заблокированный ничего не
// узнаёт и продолжает писать другим. Себя заблокировать нельзя — Store молча
// игнорирует такой вызов.
func handleBlock(store *Store, notify *Notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d BlockData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" || d.UserID <= 0 {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии и id пользователя")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("block session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		if d.Unblock {
			if err := store.UnblockUser(u.ID, d.UserID); err != nil {
				slog.Error("unblock", "err", err, "user_id", u.ID, "target", d.UserID)
				writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось снять блокировку")
				return
			}
			slog.Info("user unblocked", "user_id", u.ID, "target", d.UserID)
			writeJSON(w, http.StatusOK, struct{}{})
			return
		}
		if err := store.BlockUser(u.ID, d.UserID); err != nil {
			slog.Error("block", "err", err, "user_id", u.ID, "target", d.UserID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось заблокировать")
			return
		}
		slog.Info("user blocked", "user_id", u.ID, "target", d.UserID)
		// Apple требует, чтобы блокировка уведомляла разработчика: это сигнал о
		// проблемном человеке даже без жалобы. В горутине — ответ клиенту не
		// должен ждать Telegram, блокировка уже записана.
		if notify != nil {
			go notify.BlockToChannel(u.ID, d.UserID, d.MessageText)
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handleBlocked — GET /blocked?token= → 200 {"users": [...]} | 401 bad_session —
// кого этот человек заблокировал, с именами и аватарами.
//
// Отдельный запрос, а не поле в authed: там нужны только id (по ним клиент
// прячет живую ленту), профили же нужны ровно в одном месте — на экране
// «Заблокированные», и тянуть их при каждом входе незачем. GET, потому что это
// чтение без побочных эффектов; токен в query — как у /history.
func handleBlocked(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use GET")
			return
		}
		token := r.URL.Query().Get("token")
		if token == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии")
			return
		}
		u, err := store.UserBySession(token)
		if err != nil {
			slog.Error("blocked session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		users, err := store.BlockedUsers(u.ID)
		if err != nil {
			slog.Error("blocked users", "err", err, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось получить список")
			return
		}
		writeJSON(w, http.StatusOK, BlockedData{Users: users})
	}
}

// handleDeleteAccount — POST /account/delete {token} → 200 {} — удаление
// аккаунта: сносит пользователя, все его сессии (все устройства) и все
// сообщения (каскадом, см. Store.DeleteUser), необратимо. Требует валидную
// сессию (401 bad_session иначе).
func handleDeleteAccount(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d DeleteAccountData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("delete_account session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		if err := store.DeleteUser(u.ID); err != nil {
			slog.Error("delete_account", "err", err, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось удалить аккаунт")
			return
		}
		slog.Info("account deleted", "user_id", u.ID)
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// Куда уводит /app. У Apple в ссылке отдельный номер приложения, у Google —
// applicationId сборки (net.nous.ether). Локаль ни там, ни там не указываем:
// стор сам выберет витрину страны пользователя.
//
// Пустой playURL здесь когда-то означал «Android-сборки в Play ещё нет, уводим
// на лендинг»; проверка на пустоту в хендлере от тех времён и осталась — она
// же страхует опечатку, при которой человек попадёт на лендинг, а не в никуда.
const (
	appStoreURL = "https://apps.apple.com/app/id6790150915"
	playURL     = "https://play.google.com/store/apps/details?id=net.nous.ether"
	landingURL  = "/"
)

// Значения app_access.outcome — куда фактически отправили.
const (
	outcomeAppStore = "appstore"
	outcomePlay     = "play"
	outcomeLanding  = "landing"
)

// Значения app_access.platform — догадка о клиенте по User-Agent.
const (
	platformIOS     = "ios"
	platformAndroid = "android"
	platformDesktop = "desktop"
	platformUnknown = "unknown"
)

// Границы для строк из запроса: эндпоинт открытый и без аутентификации, а всё,
// что в него пришло, попадает в базу. Живой User-Agent в эти рамки укладывается
// с запасом, так что режется только мусор.
const (
	maxSrcLen  = 32
	maxUALen   = 512
	maxLangLen = 64
)

// handleAppLink — GET /app?src=&uid= → 302 в App Store, Google Play или на
// лендинг; попутно пишет строку в app_access.
//
// Это ссылка установки: её отдаёт кнопка «Пригласить» в приложении и QR-код.
// Единственный здесь незалогиненный хендлер, отвечающий редиректом, а не JSON, —
// на той стороне обычный браузер, а не клиент Эфира.
//
// Запись в лог не должна мешать переходу: ошибку вставки логируем, а человека
// всё равно отправляем в стор. Статистика важнее пользы от неё не бывает.
//
// Ссылка одновременно зарезервирована под universal link (namespace /app/*, см.
// ether-web). Когда подключим apple-app-site-association, у кого приложение уже
// установлено — ссылка откроется прямо в нём, сюда запрос не придёт, и в
// app_access останутся только те, у кого Эфира нет. Для метрики приглашений это
// и нужно.
//
// Отдельная неточность, о которой стоит помнить при чтении цифр: превью ссылок в
// мессенджерах (Telegram, WhatsApp) сами дёргают URL, и такие заходы тоже
// попадают в таблицу. Узнать их можно по platform=unknown — у ботов нет
// браузерного User-Agent.
func handleAppLink(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// HEAD разрешён вместе с GET: им ходят те самые сборщики превью, и
		// отвечать им ошибкой незачем.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use GET")
			return
		}
		q := r.URL.Query()
		// uid кривой или отсутствует — не повод отказывать в переходе: человек
		// пришёл ставить приложение, а приглашение просто останется неизвестным.
		uid, _ := strconv.ParseInt(q.Get("uid"), 10, 64)
		ua := r.Header.Get("User-Agent")

		platform := detectPlatform(ua)
		target, outcome := landingURL, outcomeLanding
		switch platform {
		case platformIOS:
			target, outcome = appStoreURL, outcomeAppStore
		case platformAndroid:
			if playURL != "" {
				target, outcome = playURL, outcomePlay
			}
		}

		if err := store.SaveAppAccess(AppAccess{
			UID:      uid,
			Src:      truncate(q.Get("src"), maxSrcLen),
			Platform: platform,
			Outcome:  outcome,
			UA:       truncate(ua, maxUALen),
			Lang:     truncate(r.Header.Get("Accept-Language"), maxLangLen),
			IP:       clientIP(r),
		}); err != nil {
			slog.Error("app access", "err", err, "uid", uid)
		}

		// Редирект кэшировать нельзя: закэшированный переход не дойдёт до
		// сервера, и следующие заходы просто не посчитаются.
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	}
}

// detectPlatform разбирает User-Agent настолько, насколько нужно для выбора
// стора. Точности здесь не требуется: сырой UA лежит в app_access.ua, и разбор
// всегда можно переделать задним числом.
//
// iPad в Safari по умолчанию представляется десктопным Macintosh, поэтому часть
// планшетов попадёт в desktop. Это осознанно: отличать их пришлось бы по
// косвенным признакам, а цена ошибки — лендинг вместо App Store.
func detectPlatform(ua string) string {
	if ua == "" {
		return platformUnknown
	}
	s := strings.ToLower(ua)
	switch {
	case strings.Contains(s, "android"):
		return platformAndroid
	case strings.Contains(s, "iphone"), strings.Contains(s, "ipad"), strings.Contains(s, "ipod"):
		return platformIOS
	case strings.Contains(s, "windows"), strings.Contains(s, "macintosh"),
		strings.Contains(s, "x11"), strings.Contains(s, "linux"):
		return platformDesktop
	default:
		return platformUnknown
	}
}

// clientIP достаёт адрес посетителя. На проде запрос приходит через свой Caddy,
// поэтому r.RemoteAddr — это всегда 127.0.0.1, а настоящий адрес лежит в
// X-Forwarded-For.
//
// Берём ПОСЛЕДНИЙ элемент списка, а не первый: заголовок дописывается по цепочке
// прокси, и последним его дописал наш собственный Caddy. Клиент может прислать
// свой X-Forwarded-For с любым содержимым — оно окажется в начале списка, и
// первый элемент подделывается тривиально.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// handleAuth — POST /auth/{telegram|apple|google} {id_token, name?} → 200 authed
// (user + свежий token сессии + rules_accepted) | 401 bad_auth (токен не прошёл
// проверку) | 403 banned | 400 bad_data | 501 provider_disabled (провайдер не
// задан в конфиге). id_token проверяется по публичным ключам провайдера (см.
// oidc.go), сети к самому провайдеру для этого не нужно.
//
// Один хендлер на всех: после проверки токена провайдер перестаёт иметь значение
// — дальше аккаунт живёт под внутренним id (см. identities в store.go).
//
// v == nil — провайдер не сконфигурирован. Отвечаем 501 и внятным текстом:
// клиент показывает его пользователю как есть, и по нему сразу видно, что
// сломана настройка сервера, а не вход у человека.
func handleAuth(store *Store, provider string, v *Verifier, notify *Notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		if v == nil {
			slog.Warn("auth provider not configured", "provider", provider)
			writeRESTError(w, http.StatusNotImplemented, "provider_disabled",
				"Вход через "+provider+" не настроен на сервере")
			return
		}
		var d AuthRequest
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.IDToken == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "missing id_token")
			return
		}
		pu, err := v.Verify(d.IDToken, d.Name)
		if err != nil {
			slog.Warn("auth verify failed", "err", err, "provider", provider)
			writeRESTError(w, http.StatusUnauthorized, "bad_auth", "Проверка входа не прошла")
			return
		}
		// Бан проверяем ДО создания аккаунта и по личности у провайдера: именно
		// так постоянный бан переживает удаление аккаунта — иначе нарушитель
		// удалил бы аккаунт и завёл новый тем же Apple ID. Вход закрываем только
		// при ПОСТОЯННОМ бане: временный — это мьют, человек продолжает читать и
		// заходить, отбивается лишь отправка.
		_, until, permanent, reason, err := store.BanStatusForIdentity(pu.Provider, pu.UID)
		if err != nil {
			slog.Error("auth ban check", "err", err, "provider", provider)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось проверить аккаунт")
			return
		}
		if permanent {
			slog.Info("banned login attempt", "provider", provider)
			writeRESTError(w, http.StatusForbidden, "banned", BanMessage(until, permanent, reason))
			return
		}
		// повторный вход обновляет профиль; если такой личности ещё нет — регистрация
		userID, created, accepted, err := store.UpsertByIdentity(*pu)
		if err != nil {
			slog.Error("auth upsert user", "err", err, "provider", provider)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось сохранить пользователя")
			return
		}
		// профиль читаем из БД, а не из токена: у Apple второй вход приходит без
		// имени и без фото, а показать надо то, что сохранено с первого раза
		u, err := store.UserByID(userID)
		if err != nil || u == nil {
			slog.Error("auth load user", "err", err, "user_id", userID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось прочитать профиль")
			return
		}
		if created {
			slog.Info("account created", "user_id", userID, "provider", provider)
			if notify != nil {
				go notify.AccountCreated(*u, provider)
			}
		}
		token, err := store.NewSession(userID)
		if err != nil {
			slog.Error("auth new session", "err", err, "user_id", userID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось создать сессию")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User:          authedUser(store, u),
			Token:         token,
			RulesAccepted: accepted,
		})
	}
}

// startedAt — момент запуска процесса, для uptime в /health.
var startedAt = time.Now()

// handleHealth — GET /health → 200 {ok, version, db, uptime_sec} | 503, если
// база не отвечает. Без авторизации: это цель для внешнего пингера (проверять
// живость надо снаружи — скрипт на упавшем сервере промолчит) и для
// health-check в scripts/deploy.sh.
//
// Внешние зависимости (Nominatim, FCM, Telegram) здесь СПЕЦИАЛЬНО не проверяем:
// их недоступность не значит, что сервер надо перезапускать, а пингер начал бы
// поднимать тревогу из-за чужих сбоев. За ними — алерты в лог/канал.
func handleHealth(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use GET")
			return
		}
		data := HealthData{
			OK:        true,
			Version:   version,
			DB:        "ok",
			UptimeSec: int64(time.Since(startedAt).Seconds()),
		}
		status := http.StatusOK
		if err := store.Ping(); err != nil {
			slog.Error("health: db", "err", err)
			data.OK, data.DB, status = false, err.Error(), http.StatusServiceUnavailable
		}
		writeJSON(w, status, data)
	}
}

// handleHistory — GET /history?channel=&before_id=&limit= → 200 {channel,
// messages}; без авторизации (историю можно читать не входя, как и locate).
// channel — query-параметр, а не сегмент пути: ID канала сам может содержать
// "/" (osm_type/osm_id, напр. "relation/2555133") и сломает роутинг по пути.
func handleHistory(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use GET")
			return
		}
		q := r.URL.Query()
		channel := q.Get("channel")
		if channel == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "missing channel")
			return
		}
		limit := 50
		if v := q.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		if limit <= 0 {
			limit = 50
		}
		if limit > maxHistoryLimit {
			limit = maxHistoryLimit
		}
		var beforeID int64
		if v := q.Get("before_id"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				beforeID = n
			}
		}
		// Токен опционален: читать историю можно и не входя. Но если он прислан,
		// из выборки уходят заблокированные этим человеком авторы — иначе
		// заблокированный возвращался бы при каждой подгрузке истории.
		var viewerID int64
		if token := q.Get("token"); token != "" {
			if u, err := store.UserBySession(token); err != nil {
				slog.Error("history session lookup", "err", err)
			} else if u != nil {
				viewerID = u.ID
			}
		}
		msgs, err := store.History(channel, beforeID, limit, viewerID)
		if err != nil {
			slog.Error("history", "err", err, "channel", channel)
			writeRESTError(w, http.StatusInternalServerError, "internal", "history lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, HistoryData{Channel: channel, Messages: msgs})
	}
}

// handleLink — POST /profile/link/{telegram|apple|google} {token, id_token, name?}
// → 200 authed | 401 bad_session | 401 bad_auth | 403 banned | 409 identity_taken
// | 501 provider_disabled — привязать к текущему аккаунту ещё один способ входа.
//
// Зачем: аккаунт, созданный через Apple, не имеет ни @username, ни аватара — а
// привязав Telegram, человек получает и то и другое (и на его сообщениях
// появляется «Открыть в Telegram»). Обратный смысл тоже есть: второй способ
// входа — это страховка на случай потери доступа к первому.
//
// Чего эндпоинт НЕ делает — не сливает аккаунты. Если предъявленная личность уже
// принадлежит другому аккаунту, отвечаем 409: слияние означало бы перенос
// сообщений, каналов и наказаний, то есть неизбежные потери, и молча делать это
// нельзя.
//
// Забаненную личность привязать нельзя (403): наказание считается по всем
// личностям аккаунта, так что привязка мгновенно перенесла бы бан на этот
// аккаунт — вместо этого честно отказываем.
func handleLink(store *Store, provider string, v *Verifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		if v == nil {
			writeRESTError(w, http.StatusNotImplemented, "provider_disabled",
				"Вход через "+provider+" не настроен на сервере")
			return
		}
		var d LinkRequest
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" || d.IDToken == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии и id_token")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("link session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		pu, err := v.Verify(d.IDToken, d.Name)
		if err != nil {
			slog.Warn("link verify failed", "err", err, "provider", provider)
			writeRESTError(w, http.StatusUnauthorized, "bad_auth", "Проверка входа не прошла")
			return
		}
		banned, until, permanent, reason, err := store.BanStatusForIdentity(pu.Provider, pu.UID)
		if err != nil {
			slog.Error("link ban check", "err", err, "provider", provider)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось проверить аккаунт")
			return
		}
		if banned {
			slog.Info("link of banned identity", "provider", provider, "user_id", u.ID)
			writeRESTError(w, http.StatusForbidden, "banned", BanMessage(until, permanent, reason))
			return
		}
		if err := store.LinkIdentity(u.ID, *pu); errors.Is(err, ErrIdentityTaken) {
			writeRESTError(w, http.StatusConflict, "identity_taken",
				"Этот "+provider+" уже привязан к другому аккаунту Эфира. "+
					"Войди через него или сначала удали тот аккаунт")
			return
		} else if err != nil {
			slog.Error("link identity", "err", err, "provider", provider, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось привязать аккаунт")
			return
		}
		slog.Info("identity linked", "provider", provider, "user_id", u.ID)
		// перечитываем: привязка могла дозаполнить @username, имя и аватар
		fresh, err := store.UserByID(u.ID)
		if err != nil || fresh == nil {
			slog.Error("link reload user", "err", err, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось прочитать профиль")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User:          authedUser(store, fresh),
			RulesAccepted: fresh.RulesAccepted,
		})
	}
}

// handleSetName — POST /profile/name {token, name} → 200 authed | 401 bad_session
// | 400 bad_data (пустое имя) — задать отображаемое имя вручную.
//
// Нужен потому, что имя есть не у всех провайдеров: Apple отдаёт его только при
// первой авторизации и вне токена (см. oidc.go), так что аккаунт может остаться
// безымянным — и починить это, кроме как спросив человека, нечем. Клиент
// показывает экран ввода имени в онбординге, когда сервер вернул пустое `name`.
//
// Имя нормализуется тем же cleanName, что и присланное при входе: без переводов
// строк и не длиннее maxNameLen. Ответ — обычный шейп authed, чтобы клиент взял
// итоговое (уже обрезанное) значение с сервера, а не додумывал своё.
func handleSetName(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d SetNameData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии")
			return
		}
		name := cleanName(d.Name)
		if name == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Имя не может быть пустым")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("set_name session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		if err := store.SetUserName(u.ID, name); err != nil {
			slog.Error("set_name", "err", err, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось сохранить имя")
			return
		}
		slog.Info("name set", "user_id", u.ID)
		u.FullName = name
		writeJSON(w, http.StatusOK, AuthedData{
			User:          authedUser(store, u),
			RulesAccepted: u.RulesAccepted,
		})
	}
}

// handlePushRegister — POST /push/register {token, fcm_token, platform} → 200 {}
// — привязывает токен устройства FCM к аккаунту, чтобы сервер мог адресно слать
// пуши о новых сообщениях (и НЕ слать автору его же сообщение). Требует
// валидную сессию. Идемпотентен: повторная регистрация того же токена только
// обновляет привязку (в т.ч. переносит токен на другой аккаунт).
func handlePushRegister(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d PushTokenData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" || d.FCMToken == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии и токен устройства")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("push_register session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		if err := store.SaveDeviceToken(u.ID, d.FCMToken, d.Platform); err != nil {
			slog.Error("push_register", "err", err, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось включить уведомления")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handlePushUnregister — POST /push/unregister {fcm_token} → 200 {} всегда
// (идемпотентно, как logout: важно лишь, что устройство больше не получает
// пуши). Сессия не требуется: вызывается при выходе, когда токен уже отозван, —
// а знание самого fcm_token достаточно для того, чтобы отписать это устройство.
func handlePushUnregister(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d PushTokenData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.FCMToken == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен устройства")
			return
		}
		if err := store.DeleteDeviceTokens([]string{d.FCMToken}); err != nil {
			slog.Error("push_unregister", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось отключить уведомления")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// reportReasons — допустимые коды причин жалобы (набор фиксирован клиентом,
// свободного текста нет). Неизвестный или пустой код нормализуем в "other",
// чтобы в БД не оседал мусор, но и старый клиент не ломался.
var reportReasons = map[string]bool{"spam": true, "abuse": true, "illegal": true, "other": true}

// handleReport — POST /report {token, message_id, reason} → 200 {} — жалоба на
// сообщение (модерация UGC, требование Apple 1.2). Требует валидную сессию;
// текст и автора сервер берёт из самого сообщения. 404 not_found — сообщения
// нет (удалено по TTL или неверный id). Повторная жалоба на то же сообщение —
// тоже 200: для пользователя это успех, дубля в БД не возникает.
//
// Новая жалоба уходит в служебный Telegram-канал (notify) — в горутине: ответ
// клиенту не должен ждать Telegram, а жалоба к этому моменту уже в БД.
func handleReport(store *Store, notify *Notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d ReportData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" || d.MessageID <= 0 {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "Нужен токен сессии и id сообщения")
			return
		}
		if !reportReasons[d.Reason] {
			d.Reason = "other"
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("report session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		rep, err := store.ReportMessage(d.MessageID, u.ID, d.Reason)
		if err != nil {
			slog.Error("report", "err", err, "message_id", d.MessageID, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось отправить жалобу")
			return
		}
		if rep == nil {
			writeRESTError(w, http.StatusNotFound, "not_found", "Сообщение не найдено — возможно, оно уже удалено")
			return
		}
		// повторный тап того же человека по тому же сообщению (UNIQUE в reports) —
		// не новое событие модерации: ни строки в лог, ни поста в канал, но для
		// клиента это успех, жалоба уже принята.
		if !rep.Fresh {
			slog.Info("report notify skipped", "message_id", d.MessageID, "reason", "duplicate")
			writeJSON(w, http.StatusOK, struct{}{})
			return
		}
		// лог — вторая точка входа модерации (первая — канал в Telegram)
		slog.Info("message reported", "message_id", d.MessageID, "reason", d.Reason, "reporter", u.ID)
		if notify == nil {
			// иначе «жалоба в логе есть, а поста нет» выглядит как потерянное уведомление
			slog.Info("report notify skipped", "message_id", d.MessageID, "reason", "disabled")
		} else {
			go notify.ReportToChannel(rep, u)
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handleAcceptRules — POST /rules/accept {token} → 200 authed
// {rules_accepted: true} | 401 bad_session | 400 not_authed (нет токена).
func handleAcceptRules(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d AcceptRulesData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" {
			writeRESTError(w, http.StatusBadRequest, "not_authed", "Нужен токен сессии")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("accept_rules session lookup", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		if err := store.AcceptRules(u.ID); err != nil {
			slog.Error("accept_rules", "err", err, "user_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "failed to save rules acceptance")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User:          authedUser(store, u),
			RulesAccepted: true,
		})
	}
}

// handleLogout — POST /session/logout {token} → 200 {} всегда (идемпотентно:
// отзыв несуществующего токена — тоже успех, клиенту важно лишь «сессии больше
// нет»). Отзывает только этот токен, другие устройства пользователя не трогает.
func handleLogout(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d LogoutData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "invalid logout payload")
			return
		}
		if err := store.DeleteSession(d.Token); err != nil {
			slog.Error("logout", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "logout failed")
			return
		}
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handleResume — POST /session/resume {token} → 200 authed (name/username/
// rules_accepted, без token — клиент его и так прислал) | 401 bad_session.
func handleResume(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d ResumeData
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Token == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "invalid resume payload")
			return
		}
		u, err := store.UserBySession(d.Token)
		if err != nil {
			slog.Error("resume", "err", err)
			writeRESTError(w, http.StatusInternalServerError, "internal", "session lookup failed")
			return
		}
		if u == nil {
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди заново")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User:          authedUser(store, u),
			RulesAccepted: u.RulesAccepted,
		})
	}
}

// ─── общие хелперы ответа ───

// authedUser собирает ответ про личность. Providers (какие способы входа
// привязаны) читаются из БД: по ним клиент решает, показывать ли кнопку
// «Привязать Telegram». Ошибка чтения не повод ронять ответ — это подсказка для
// UI, а не сама личность, поэтому в худшем случае список будет пустым.
func authedUser(store *Store, u *User) AuthedUser {
	var providers []string
	ids, err := store.Identities(u.ID)
	if err != nil {
		slog.Error("identities", "err", err, "user_id", u.ID)
	}
	for _, id := range ids {
		providers = append(providers, id.Provider)
	}
	sort.Strings(providers) // стабильный порядок: клиент сравнивает списки
	blocked, err := store.BlockedBy(u.ID)
	if err != nil {
		slog.Error("blocked by", "err", err, "user_id", u.ID)
	}
	return AuthedUser{
		ID:        u.ID,
		Username:  u.TgUsername,
		Name:      u.FullName,
		AvatarURL: u.AvatarURL,
		Providers: providers,
		Blocked:   blocked,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeRESTError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorData{Code: code, Message: message})
}
