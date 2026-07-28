package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // чистый Go, без cgo
)

// Store — персистентность в SQLite (файл `db` из конфига, свой на окружение):
// пользователи, сессии и сообщения каналов.
//
// Пользователь создаётся/обновляется при каждом входе через Telegram (ключ —
// tg id). Сессия — случайный токен, который клиент получает в `authed` и
// предъявляет в `resume` после реконнекта, чтобы не ходить к боту заново.
// Сообщение хранится под ключом-строкой ID канала; история отдаётся кадром
// `history` страницами вверх (before_id).
type Store struct {
	db *sql.DB
}

type User struct {
	TgID       int64
	TgUsername string // @username — для ссылки на профиль в Telegram (не для имени)
	FullName   string // отображаемое имя (Telegram `name`); единственное для UI
	AvatarURL  string // URL фото профиля из Telegram (claim `picture`); может быть пустым
	// RulesAccepted — согласие с правилами Эфира привязано к Telegram-аккаунту,
	// а не к устройству/сессии: однажды принял — экран правил больше не увидит,
	// даже переустановив клиент или потеряв shared_preferences.
	RulesAccepted bool
	// Banned — активное наказание модератора: отправка сообщений запрещена
	// («мьют»). Чтение и вход остаются — временный бан никого не выселяет.
	Banned bool
	// BanPermanent — постоянный бан (повторное нарушение). Аккаунт при этом
	// удалён, и вход запрещён совсем: иначе нарушитель просто зарегистрировался
	// бы заново под тем же Telegram id.
	BanPermanent bool
}

const storeSchema = `
CREATE TABLE IF NOT EXISTS users (
	tg_id             INTEGER PRIMARY KEY, -- Telegram user id
	tg_username       TEXT NOT NULL DEFAULT '', -- @username (ссылка на профиль)
	full_name         TEXT NOT NULL DEFAULT '', -- отображаемое имя (для UI)
	avatar_url        TEXT NOT NULL DEFAULT '', -- URL фото профиля из Telegram
	created_at        INTEGER NOT NULL,    -- unix-миллисекунды (как всюду в БД)
	seen_at           INTEGER NOT NULL,
	rules_accepted_at INTEGER NOT NULL DEFAULT 0 -- unix-мс; 0 — не принял
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	-- ON DELETE CASCADE: удаление аккаунта (DeleteUser) само сносит все его
	-- сессии; работает при foreign_keys=ON (см. OpenStore).
	tg_id      INTEGER NOT NULL REFERENCES users(tg_id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL, -- unix-миллисекунды
	seen_at    INTEGER NOT NULL  -- unix-миллисекунды
);
CREATE INDEX IF NOT EXISTS sessions_tg_id ON sessions(tg_id);
CREATE TABLE IF NOT EXISTS messages (
	id      INTEGER PRIMARY KEY AUTOINCREMENT, -- монотонный, курсор пагинации
	channel TEXT NOT NULL,                     -- ID канала (контракт ether-meta)
	-- автор; имя/аватар — JOIN из users. ON DELETE CASCADE: удаление аккаунта
	-- стирает и его сообщения (при foreign_keys=ON).
	tg_id   INTEGER NOT NULL REFERENCES users(tg_id) ON DELETE CASCADE,
	text    TEXT NOT NULL,
	ts      INTEGER NOT NULL                   -- unix-миллисекунды (контракт протокола)
);
CREATE INDEX IF NOT EXISTS messages_channel_id ON messages(channel, id);
-- под уборку старых сообщений по TTL (DeleteMessagesOlderThan)
CREATE INDEX IF NOT EXISTS messages_ts ON messages(ts);
-- Жалобы на сообщения (модерация UGC — требование Apple 1.2). Храним копию
-- текста и автора: сообщение живёт messageTTL (неделя) и удаляется, а жалоба
-- должна остаться разбираемой, поэтому ссылку на messages не ставим — id
-- сохраняем справочно, без FK.
CREATE TABLE IF NOT EXISTS reports (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id     INTEGER NOT NULL,        -- id сообщения на момент жалобы
	channel        TEXT NOT NULL,           -- где было сообщение
	author_tg_id   INTEGER NOT NULL,        -- на кого жалуются
	message_text   TEXT NOT NULL,           -- копия: сообщение может быть удалено
	reporter_tg_id INTEGER NOT NULL REFERENCES users(tg_id) ON DELETE CASCADE,
	reason         TEXT NOT NULL DEFAULT '',-- код причины из клиента
	created_at     INTEGER NOT NULL,        -- unix-миллисекунды
	-- повторная жалоба того же человека на то же сообщение — не новая запись
	UNIQUE(message_id, reporter_tg_id)
);
CREATE INDEX IF NOT EXISTS reports_created_at ON reports(created_at);
CREATE INDEX IF NOT EXISTS reports_author ON reports(author_tg_id);
-- Адресная доставка пушей (см. push.go). Топики FCM не годятся: в топик нельзя
-- НЕ отправить конкретному подписчику, из-за чего автор получал уведомление о
-- своём же сообщении. Поэтому сервер держит токены устройств и каналы каждого
-- пользователя и шлёт адресно, исключая отправителя.
CREATE TABLE IF NOT EXISTS device_tokens (
	fcm_token  TEXT PRIMARY KEY,        -- токен устройства (он же ключ: при
	                                    -- переустановке/смене аккаунта токен
	                                    -- переезжает к другому tg_id)
	tg_id      INTEGER NOT NULL REFERENCES users(tg_id) ON DELETE CASCADE,
	platform   TEXT NOT NULL DEFAULT '',-- ios|android, для диагностики
	updated_at INTEGER NOT NULL         -- unix-миллисекунды
);
CREATE INDEX IF NOT EXISTS device_tokens_tg_id ON device_tokens(tg_id);
-- Каналы пользователя: обновляются на каждый locate (сервер и так их вычисляет).
-- Нужны, чтобы понять, кому слать пуш, когда WS-соединения уже нет.
CREATE TABLE IF NOT EXISTS user_channels (
	tg_id      INTEGER NOT NULL REFERENCES users(tg_id) ON DELETE CASCADE,
	channel    TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tg_id, channel)
);
CREATE INDEX IF NOT EXISTS user_channels_channel ON user_channels(channel);
-- Наказания модератора. Ключ — Telegram id, и таблица СПЕЦИАЛЬНО без FK на
-- users: вход идёт по Telegram-аккаунту, поэтому удаление аккаунта из users не
-- должно снимать наказание — иначе нарушитель просто войдёт заново и получит
-- чистый профиль.
--
-- Автоматической эскалации нет: решает модератор. Он видит count (сколько раз
-- наказывали) в посте жалобы и сам выбирает мьют на сутки или постоянный бан.
CREATE TABLE IF NOT EXISTS bans (
	tg_id      INTEGER PRIMARY KEY,          -- кого наказали (может уже не быть в users)
	until      INTEGER NOT NULL DEFAULT 0,   -- unix-мс, до когда временный бан; 0 — нет
	permanent  INTEGER NOT NULL DEFAULT 0,   -- 1 — постоянный запрет входа
	count      INTEGER NOT NULL DEFAULT 0,   -- сколько раз наказывали (справочно, для модератора)
	reason     TEXT NOT NULL DEFAULT '',     -- причина последнего наказания (показывается пользователю)
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// один писатель: сериализуем соединения, чтобы не ловить SQLITE_BUSY
	db.SetMaxOpenConns(1)
	// WAL: читатели не блокируют писателя, и коммиты заметно дешевле
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(storeSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Миграций нет: до closed-beta MVP схему меняем свободно, а БД пересоздаём
	// с нуля (единственный источник схемы — storeSchema выше).
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping — проверка живости базы для /health. Именно запрос, а не sql.DB.Ping():
// у SQLite соединение считается живым почти всегда, а вот что файл на месте и
// схема читается, покажет только настоящий SELECT.
func (s *Store) Ping() error {
	var one int
	return s.db.QueryRow(`SELECT 1`).Scan(&one)
}

// CreateUser заводит нового пользователя (ключ — tg id). Правила при
// регистрации не приняты — их отмечает только AcceptRules.
func (s *Store) CreateUser(u User) error {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO users (tg_id, tg_username, full_name, avatar_url, created_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		u.TgID, u.TgUsername, u.FullName, u.AvatarURL, now, now)
	return err
}

// UpdateUser обновляет профиль существующего пользователя: tg_username, имя и
// аватар подхватываются заново при каждом входе, seen_at освежается.
// rules_accepted_at не трогает — его меняет только AcceptRules.
//
// found=false — такого пользователя ещё нет (значит вход первый, зовите
// CreateUser). rulesAccepted — принимал ли он правила раньше; RETURNING отдаёт
// это тем же запросом, что и UPDATE.
func (s *Store) UpdateUser(u User) (found, rulesAccepted bool, err error) {
	var acceptedAt int64
	err = s.db.QueryRow(`
		UPDATE users SET
			tg_username = ?,
			full_name = ?,
			avatar_url = ?,
			seen_at = ?
		WHERE tg_id = ?
		RETURNING rules_accepted_at`,
		u.TgUsername, u.FullName, u.AvatarURL, time.Now().UnixMilli(), u.TgID,
	).Scan(&acceptedAt)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, acceptedAt > 0, nil
}

// AcceptRules отмечает, что пользователь принял правила Эфира — привязано к
// Telegram-аккаунту, переживает переустановку клиента и смену устройства.
func (s *Store) AcceptRules(tgID int64) error {
	_, err := s.db.Exec(`UPDATE users SET rules_accepted_at = ? WHERE tg_id = ?`, time.Now().UnixMilli(), tgID)
	return err
}

// NewSession выпускает токен сессии для пользователя. Токенов может быть
// несколько (несколько устройств); срока жизни пока нет — прототип.
func (s *Store) NewSession(tgID int64) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO sessions (token, tg_id, created_at, seen_at) VALUES (?, ?, ?, ?)`,
		token, tgID, now, now)
	if err != nil {
		return "", err
	}
	return token, nil
}

// DeleteSession отзывает один токен сессии (логаут). Идемпотентна: отсутствие
// токена — не ошибка. Другие устройства того же пользователя не трогает.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// DeleteUser удаляет аккаунт целиком, необратимо: сам пользователь + каскадом
// (ON DELETE CASCADE, см. схему) все его сессии (все устройства) и сообщения.
// Идемпотентна: удаление отсутствующего tg_id — не ошибка.
func (s *Store) DeleteUser(tgID int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE tg_id = ?`, tgID)
	return err
}

// SaveMessage пишет сообщение в историю канала и возвращает его id.
func (s *Store) SaveMessage(channel string, tgID int64, text string, ts int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO messages (channel, tg_id, text, ts) VALUES (?, ?, ?, ?)`,
		channel, tgID, text, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteMessagesOlderThan удаляет сообщения старше ttl и возвращает их число.
// Зовётся по таймеру (см. cleanup.go): чат географический и живой, старая
// история никому не нужна, а база не должна расти бесконечно.
func (s *Store) DeleteMessagesOlderThan(ttl time.Duration) (int64, error) {
	cutoff := time.Now().Add(-ttl).UnixMilli()
	res, err := s.db.Exec(`DELETE FROM messages WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// History возвращает до limit последних сообщений канала в хронологическом
// порядке (по возрастанию id). beforeID > 0 — страница вверх: только сообщения
// старше него.
func (s *Store) History(channel string, beforeID int64, limit int) ([]MessageData, error) {
	// имя, @username и аватар автора — JOIN из users по tg_id (в messages их
	// нет). LEFT JOIN — защитно: при удалении аккаунта сообщения удаляются
	// каскадом вместе с автором (см. DeleteUser), поэтому «висячих» строк без
	// users быть не должно, но JOIN не должен ронять выборку, если что.
	q := `SELECT m.id, m.channel, m.tg_id, COALESCE(u.full_name, ''), COALESCE(u.tg_username, ''), COALESCE(u.avatar_url, ''), m.text, m.ts
		FROM messages m LEFT JOIN users u ON u.tg_id = m.tg_id
		WHERE m.channel = ?`
	args := []any{channel}
	if beforeID > 0 {
		q += ` AND m.id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY m.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := make([]MessageData, 0, limit)
	for rows.Next() {
		var m MessageData
		if err := rows.Scan(
			&m.ID, &m.Channel, &m.SenderID, &m.Sender, &m.Username,
			&m.AvatarURL, &m.Text, &m.TS,
		); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// выборка шла новые→старые, отдаём хронологически
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// ReportedMessage — что именно нажаловали: данные для разбора и для уведомления
// в служебный Telegram-канал (см. notify.go).
type ReportedMessage struct {
	MessageID      int64
	Channel        string
	AuthorTgID     int64
	AuthorName     string // имя автора на момент жалобы (JOIN из users)
	AuthorUsername string // @username автора; пусто — нет username
	Text           string
	Reason         string
	// AuthorBanCount — сколько раз автора уже наказывали. Показывается модератору
	// в посте жалобы: эскалации нет, решение (мьют или блокировка) за ним.
	AuthorBanCount int
	// Fresh — жалоба записана впервые. false: этот пользователь уже жаловался на
	// это сообщение — в БД дубля нет, и уведомление повторно слать не надо
	// (иначе повторные тапы засорят канал).
	Fresh bool
}

// ReportMessage регистрирует жалобу на сообщение. Текст и автора копируем из
// самого сообщения (клиент их не присылает — иначе на что жалуются, решал бы
// он): сообщение живёт неделю, а жалоба должна остаться разбираемой после его
// удаления. Возвращает nil, если сообщения с таким id нет (уже удалено по TTL
// или id выдуман). Повторная жалоба того же пользователя на то же сообщение —
// не ошибка и не дубль (UNIQUE + INSERT OR IGNORE), для клиента это успех.
func (s *Store) ReportMessage(messageID, reporterTgID int64, reason string) (*ReportedMessage, error) {
	rep := &ReportedMessage{MessageID: messageID, Reason: reason}
	// LEFT JOIN: имя автора — удобство для разбора, его отсутствие не должно
	// мешать принять жалобу
	err := s.db.QueryRow(`
		SELECT m.channel, m.tg_id, m.text, COALESCE(u.full_name, ''), COALESCE(u.tg_username, '')
		FROM messages m LEFT JOIN users u ON u.tg_id = m.tg_id
		WHERE m.id = ?`, messageID).
		Scan(&rep.Channel, &rep.AuthorTgID, &rep.Text, &rep.AuthorName, &rep.AuthorUsername)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO reports
			(message_id, channel, author_tg_id, message_text, reporter_tg_id, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, rep.Channel, rep.AuthorTgID, rep.Text, reporterTgID, reason, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	if rep.AuthorBanCount, err = s.BanCount(rep.AuthorTgID); err != nil {
		return nil, err
	}
	// 0 изменённых строк — сработал IGNORE, т.е. жалоба уже была
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		rep.Fresh = true
	}
	return rep, nil
}

// banDuration — срок мьюта по умолчанию. Сутки: достаточно, чтобы остудить, и не
// требует от модератора возвращаться и снимать вручную — истекает сам, без
// фоновой уборки (проверка идёт по времени, см. BanStatus).
const banDuration = 24 * time.Hour

// BanTemporary закрывает отправку на banDuration («мьют»): вход и чтение
// остаются, сессии не отзываются. reason — необязательная причина, её увидит
// пользователь. Возвращает время окончания и накопленное число наказаний, чтобы
// модератор видел, сколько раз этот человек уже попадался.
func (s *Store) BanTemporary(tgID int64, reason string) (until time.Time, count int, err error) {
	now := time.Now()
	until = now.Add(banDuration)
	if err = s.upsertBan(tgID, until.UnixMilli(), 0, reason, now); err != nil {
		return time.Time{}, 0, err
	}
	count, err = s.BanCount(tgID)
	return until, count, err
}

// BanPermanent закрывает вход навсегда и удаляет аккаунт (профиль, сессии,
// сообщения и токены устройств уходят каскадом). Запись в bans остаётся —
// именно она не даёт зарегистрироваться заново под тем же Telegram id.
// Автоматически не вызывается: решение всегда за модератором.
func (s *Store) BanPermanent(tgID int64, reason string) (count int, err error) {
	now := time.Now()
	if err = s.upsertBan(tgID, 0, 1, reason, now); err != nil {
		return 0, err
	}
	if err = s.DeleteUser(tgID); err != nil {
		return 0, err
	}
	return s.BanCount(tgID)
}

// upsertBan пишет наказание и увеличивает счётчик нарушений.
func (s *Store) upsertBan(tgID, until int64, permanent int, reason string, now time.Time) error {
	nowMs := now.UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO bans (tg_id, until, permanent, count, reason, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT(tg_id) DO UPDATE SET
			until = excluded.until,
			permanent = excluded.permanent,
			count = bans.count + 1,
			reason = excluded.reason,
			updated_at = excluded.updated_at`,
		tgID, until, permanent, reason, nowMs, nowMs)
	return err
}

// BanCount — сколько раз этого человека наказывали (включая снятые). Модератор
// видит это число в посте жалобы и решает, мьютить или банить насовсем.
func (s *Store) BanCount(tgID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count FROM bans WHERE tg_id = ?`, tgID).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// BanStatus — активно ли наказание. Временный бан истекает сам: сравниваем until
// с текущим временем, поэтому фоновая уборка не нужна. permanent игнорирует until.
func (s *Store) BanStatus(tgID int64) (banned bool, until time.Time, permanent bool, reason string, err error) {
	var untilMs int64
	var perm int
	err = s.db.QueryRow(`SELECT until, permanent, reason FROM bans WHERE tg_id = ?`, tgID).
		Scan(&untilMs, &perm, &reason)
	if err == sql.ErrNoRows {
		return false, time.Time{}, false, "", nil
	}
	if err != nil {
		return false, time.Time{}, false, "", err
	}
	if perm == 1 {
		return true, time.Time{}, true, reason, nil
	}
	if untilMs > time.Now().UnixMilli() {
		return true, time.UnixMilli(untilMs), false, reason, nil
	}
	return false, time.Time{}, false, "", nil
}

// BanMessage — что показать наказанному. Формулировка одна на все точки входа
// (логин, апгрейд сокета, publish), чтобы не разъезжалась.
func BanMessage(until time.Time, permanent bool, reason string) string {
	var msg string
	switch {
	case permanent:
		msg = "Аккаунт заблокирован за нарушение правил Эфира"
	default:
		// временный бан — мьют: читать можно, поэтому говорим про отправку
		left := time.Until(until).Round(time.Hour)
		if left < time.Hour {
			msg = "Отправка сообщений закрыта за нарушение правил Эфира. Осталось меньше часа"
		} else {
			msg = fmt.Sprintf(
				"Отправка сообщений закрыта за нарушение правил Эфира. Осталось %d ч",
				int(left.Hours()))
		}
	}
	if reason != "" {
		msg += ". Причина: " + reason
	}
	return msg
}

// Unban снимает наказание вручную (ошиблись, разобрались). Историю нарушений
// (count) НЕ сбрасывает: модератор должен видеть, сколько раз человек попадался,
// даже если прошлые наказания снимали. Возвращает false, если наказания нет.
func (s *Store) Unban(tgID int64) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE bans SET until = 0, permanent = 0, updated_at = ?
		WHERE tg_id = ? AND (until > 0 OR permanent = 1)`, time.Now().UnixMilli(), tgID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteMessage удаляет одно сообщение (реакция на жалобу). Возвращает false,
// если сообщения уже нет — оно могло уйти по TTL или быть удалено раньше.
// Жалобы на него остаются: в них хранится копия текста (см. ReportMessage).
func (s *Store) DeleteMessage(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteUserMessages удаляет все сообщения пользователя и возвращает их число —
// когда одного сообщения мало (спамер засыпал канал).
func (s *Store) DeleteUserMessages(tgID int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE tg_id = ?`, tgID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SaveDeviceToken привязывает FCM-токен устройства к пользователю (upsert по
// токену). Ключ — сам токен: при переустановке или входе другим аккаунтом FCM
// отдаёт тот же токен, и он должен «переехать» к новому tg_id, а не задвоиться.
func (s *Store) SaveDeviceToken(tgID int64, fcmToken, platform string) error {
	_, err := s.db.Exec(`
		INSERT INTO device_tokens (fcm_token, tg_id, platform, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(fcm_token) DO UPDATE SET
			tg_id = excluded.tg_id,
			platform = excluded.platform,
			updated_at = excluded.updated_at`,
		fcmToken, tgID, platform, time.Now().UnixMilli())
	return err
}

// DeleteDeviceTokens удаляет токены устройств: при выходе из аккаунта (свой
// токен) и при уборке мёртвых токенов, которые FCM отверг как UNREGISTERED.
// Идемпотентна, пустой список — no-op.
func (s *Store) DeleteDeviceTokens(fcmTokens []string) error {
	for _, t := range fcmTokens {
		if _, err := s.db.Exec(`DELETE FROM device_tokens WHERE fcm_token = ?`, t); err != nil {
			return err
		}
	}
	return nil
}

// SetUserChannels заменяет набор каналов пользователя (вызывается на locate).
// Полная замена, а не добавление: при переезде старые каналы должны исчезнуть,
// иначе пуши продолжат приходить из района, откуда пользователь уехал.
func (s *Store) SetUserChannels(tgID int64, channels []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM user_channels WHERE tg_id = ?`, tgID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, ch := range channels {
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO user_channels (tg_id, channel, updated_at) VALUES (?, ?, ?)`,
			tgID, ch, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PushTargets отдаёт токены устройств, которым надо доставить пуш о новом
// сообщении в канале: все устройства пользователей, у кого этот канал в наборе,
// **кроме** автора сообщения (exceptTgID) — иначе человек получает уведомление о
// своём же сообщении, ради чего адресная модель и вводилась.
func (s *Store) PushTargets(channel string, exceptTgID int64) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT d.fcm_token
		FROM user_channels uc JOIN device_tokens d ON d.tg_id = uc.tg_id
		WHERE uc.channel = ? AND uc.tg_id != ?`, channel, exceptTgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// UserBySession возвращает пользователя по токену сессии (nil — сессии нет)
// и отмечает сессию как живую.
func (s *Store) UserBySession(token string) (*User, error) {
	var u User
	var acceptedAt, banUntil int64
	var banPermanent int
	// LEFT JOIN bans: наказание живёт отдельно от аккаунта (см. схему), а
	// временное истекает само — поэтому сравниваем until с текущим временем
	// здесь же, а не полагаемся на фоновую уборку.
	err := s.db.QueryRow(`
		SELECT u.tg_id, u.tg_username, u.full_name, u.avatar_url, u.rules_accepted_at,
		       COALESCE(b.until, 0), COALESCE(b.permanent, 0)
		FROM sessions s
		JOIN users u ON u.tg_id = s.tg_id
		LEFT JOIN bans b ON b.tg_id = u.tg_id
		WHERE s.token = ?`, token).
		Scan(&u.TgID, &u.TgUsername, &u.FullName, &u.AvatarURL, &acceptedAt,
			&banUntil, &banPermanent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.RulesAccepted = acceptedAt > 0
	u.BanPermanent = banPermanent == 1
	u.Banned = u.BanPermanent || banUntil > time.Now().UnixMilli()
	now := time.Now().UnixMilli()
	s.db.Exec(`UPDATE sessions SET seen_at = ? WHERE token = ?`, now, token)
	s.db.Exec(`UPDATE users SET seen_at = ? WHERE tg_id = ?`, now, u.TgID)
	return &u, nil
}
