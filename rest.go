package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
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
// т.д.). Незаданный провайдер эндпоинта не получает: пустой конфиг не должен
// притворяться работающим входом.
func registerREST(mux *http.ServeMux, store *Store, verifiers map[string]*Verifier, notify *Notifier) {
	mux.HandleFunc("/account/delete", handleDeleteAccount(store))
	for provider, v := range verifiers {
		mux.HandleFunc("/auth/"+provider, handleAuth(store, provider, v, notify))
	}
	mux.HandleFunc("/health", handleHealth(store))
	mux.HandleFunc("/history", handleHistory(store))
	mux.HandleFunc("/push/register", handlePushRegister(store))
	mux.HandleFunc("/push/unregister", handlePushUnregister(store))
	mux.HandleFunc("/report", handleReport(store, notify))
	mux.HandleFunc("/rules/accept", handleAcceptRules(store))
	mux.HandleFunc("/session/logout", handleLogout(store))
	mux.HandleFunc("/session/resume", handleResume(store))
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

// handleAuth — POST /auth/{telegram|apple|google} {id_token, name?} → 200 authed
// (user + свежий token сессии + rules_accepted) | 401 bad_auth (токен не прошёл
// проверку) | 403 banned | 400 bad_data. id_token проверяется по публичным ключам
// провайдера (см. oidc.go), сети к самому провайдеру для этого не нужно.
//
// Один хендлер на всех: после проверки токена провайдер перестаёт иметь значение
// — дальше аккаунт живёт под внутренним id (см. identities в store.go).
func handleAuth(store *Store, provider string, v *Verifier, notify *Notifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
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
			User: AuthedUser{
				ID:        u.ID,
				Username:  u.TgUsername,
				Name:      u.FullName,
				AvatarURL: u.AvatarURL,
			},
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
		msgs, err := store.History(channel, beforeID, limit)
		if err != nil {
			slog.Error("history", "err", err, "channel", channel)
			writeRESTError(w, http.StatusInternalServerError, "internal", "history lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, HistoryData{Channel: channel, Messages: msgs})
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
		// лог — вторая точка входа модерации (первая — канал в Telegram)
		slog.Info("message reported", "message_id", d.MessageID, "reason", d.Reason, "reporter", u.ID)
		// только новые жалобы: повторный тап не должен дублировать пост в канале
		if notify != nil && rep.Fresh {
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
			User: AuthedUser{
				ID:        u.ID,
				Username:  u.TgUsername,
				Name:      u.FullName,
				AvatarURL: u.AvatarURL,
			},
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
			User: AuthedUser{
				ID:        u.ID,
				Username:  u.TgUsername,
				Name:      u.FullName,
				AvatarURL: u.AvatarURL,
			},
			RulesAccepted: u.RulesAccepted,
		})
	}
}

// ─── общие хелперы ответа ───

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeRESTError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorData{Code: code, Message: message})
}
