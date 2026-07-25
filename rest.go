package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
)

// REST — синхронный запрос-ответ без побочных эффектов на живом WS-соединении:
// вход через Telegram Login SDK (auth), resume, logout, удаление аккаунта,
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

func registerREST(mux *http.ServeMux, store *Store, tg *TelegramAuth) {
	mux.HandleFunc("/account/delete", handleDeleteAccount(store))
	mux.HandleFunc("/auth/telegram", handleAuthTelegram(store, tg))
	mux.HandleFunc("/history", handleHistory(store))
	mux.HandleFunc("/report", handleReport(store))
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
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди через Telegram заново")
			return
		}
		if err := store.DeleteUser(u.TgID); err != nil {
			slog.Error("delete_account", "err", err, "tg_id", u.TgID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось удалить аккаунт")
			return
		}
		slog.Info("account deleted", "tg_id", u.TgID)
		writeJSON(w, http.StatusOK, struct{}{})
	}
}

// handleAuthTelegram — POST /auth/telegram {id_token} → 200 authed (user +
// свежий token сессии + rules_accepted) | 401 bad_auth (JWT не прошёл) | 400
// bad_data. id_token — OIDC-токен от нативного Telegram Login SDK; сервер
// проверяет его подпись по публичным ключам Telegram (см. telegram.go), сети к
// api.telegram.org не нужно.
func handleAuthTelegram(store *Store, tg *TelegramAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeRESTError(w, http.StatusMethodNotAllowed, "bad_method", "use POST")
			return
		}
		var d TelegramAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.IDToken == "" {
			writeRESTError(w, http.StatusBadRequest, "bad_data", "missing id_token")
			return
		}
		u, err := tg.Verify(d.IDToken)
		if err != nil {
			slog.Warn("auth verify failed", "err", err)
			writeRESTError(w, http.StatusUnauthorized, "bad_auth", "Проверка входа Telegram не прошла")
			return
		}
		// повторный вход обновляет профиль; если пользователя ещё нет — регистрация
		user := User{TgID: u.ID, TgUsername: u.Username, FullName: u.Name, AvatarURL: u.AvatarURL}
		found, accepted, err := store.UpdateUser(user)
		if err != nil {
			slog.Error("auth update user", "err", err, "tg_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось сохранить пользователя")
			return
		}
		if !found {
			if err := store.CreateUser(user); err != nil {
				slog.Error("auth create user", "err", err, "tg_id", u.ID)
				writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось сохранить пользователя")
				return
			}
			slog.Info("account created", "tg_id", u.ID)
		}
		token, err := store.NewSession(u.ID)
		if err != nil {
			slog.Error("auth new session", "err", err, "tg_id", u.ID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось создать сессию")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User: AuthedUser{
				ID:        u.ID,
				Username:  u.Username,
				Name:      u.Name,
				AvatarURL: u.AvatarURL,
			},
			Token:         token,
			RulesAccepted: accepted,
		})
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

// reportReasons — допустимые коды причин жалобы (набор фиксирован клиентом,
// свободного текста нет). Неизвестный или пустой код нормализуем в "other",
// чтобы в БД не оседал мусор, но и старый клиент не ломался.
var reportReasons = map[string]bool{"spam": true, "abuse": true, "illegal": true, "other": true}

// handleReport — POST /report {token, message_id, reason} → 200 {} — жалоба на
// сообщение (модерация UGC, требование Apple 1.2). Требует валидную сессию;
// текст и автора сервер берёт из самого сообщения. 404 not_found — сообщения
// нет (удалено по TTL или неверный id). Повторная жалоба на то же сообщение —
// тоже 200: для пользователя это успех, дубля в БД не возникает.
func handleReport(store *Store) http.HandlerFunc {
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
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди через Telegram заново")
			return
		}
		ok, err := store.ReportMessage(d.MessageID, u.TgID, d.Reason)
		if err != nil {
			slog.Error("report", "err", err, "message_id", d.MessageID, "tg_id", u.TgID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "Не удалось отправить жалобу")
			return
		}
		if !ok {
			writeRESTError(w, http.StatusNotFound, "not_found", "Сообщение не найдено — возможно, оно уже удалено")
			return
		}
		// лог — точка входа модерации: по нему видно, на что жалуются
		slog.Info("message reported", "message_id", d.MessageID, "reason", d.Reason, "reporter", u.TgID)
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
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди через Telegram заново")
			return
		}
		if err := store.AcceptRules(u.TgID); err != nil {
			slog.Error("accept_rules", "err", err, "tg_id", u.TgID)
			writeRESTError(w, http.StatusInternalServerError, "internal", "failed to save rules acceptance")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User: AuthedUser{
				ID:        u.TgID,
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
			writeRESTError(w, http.StatusUnauthorized, "bad_session", "Сессия не найдена — войди через Telegram заново")
			return
		}
		writeJSON(w, http.StatusOK, AuthedData{
			User: AuthedUser{
				ID:        u.TgID,
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
