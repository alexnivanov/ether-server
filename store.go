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
	// RulesAccepted — согласие с правилами эфира привязано к Telegram-аккаунту,
	// а не к устройству/сессии: однажды принял — экран правил больше не увидит,
	// даже переустановив клиент или потеряв shared_preferences.
	RulesAccepted bool
}

const storeSchema = `
CREATE TABLE IF NOT EXISTS users (
	tg_id             INTEGER PRIMARY KEY, -- Telegram user id
	tg_username       TEXT NOT NULL DEFAULT '', -- @username (ссылка на профиль)
	full_name         TEXT NOT NULL DEFAULT '', -- отображаемое имя (для UI)
	avatar_url        TEXT NOT NULL DEFAULT '', -- URL фото профиля из Telegram
	created_at        INTEGER NOT NULL,    -- unix-секунды
	seen_at           INTEGER NOT NULL,
	rules_accepted_at INTEGER NOT NULL DEFAULT 0 -- 0 — не принял
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	tg_id      INTEGER NOT NULL REFERENCES users(tg_id),
	created_at INTEGER NOT NULL,
	seen_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_tg_id ON sessions(tg_id);
CREATE TABLE IF NOT EXISTS messages (
	id      INTEGER PRIMARY KEY AUTOINCREMENT, -- монотонный, курсор пагинации
	channel TEXT NOT NULL,                     -- ID канала (контракт ether-meta)
	tg_id   INTEGER NOT NULL,                  -- автор; имя и аватар берутся JOIN из users
	text    TEXT NOT NULL,
	ts      INTEGER NOT NULL                   -- unix-миллисекунды (как в протоколе)
);
CREATE INDEX IF NOT EXISTS messages_channel_id ON messages(channel, id);
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

// SaveUser создаёт или обновляет пользователя (ключ — tg id): tg_username и имя
// подхватываются заново при каждом входе; rules_accepted_at не трогает — его
// меняет только AcceptRules. Возвращает, принимал ли пользователь правила
// раньше (для повторного входа тем же Telegram-аккаунтом).
func (s *Store) SaveUser(u User) (rulesAccepted bool, err error) {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`
		INSERT INTO users (tg_id, tg_username, full_name, avatar_url, created_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tg_id) DO UPDATE SET
			tg_username = excluded.tg_username,
			full_name = excluded.full_name,
			avatar_url = excluded.avatar_url,
			seen_at = excluded.seen_at`,
		u.TgID, u.TgUsername, u.FullName, u.AvatarURL, now, now); err != nil {
		return false, err
	}
	var acceptedAt int64
	err = s.db.QueryRow(`SELECT rules_accepted_at FROM users WHERE tg_id = ?`, u.TgID).Scan(&acceptedAt)
	return acceptedAt > 0, err
}

// AcceptRules отмечает, что пользователь принял правила эфира — привязано к
// Telegram-аккаунту, переживает переустановку клиента и смену устройства.
func (s *Store) AcceptRules(tgID int64) error {
	_, err := s.db.Exec(`UPDATE users SET rules_accepted_at = ? WHERE tg_id = ?`, time.Now().Unix(), tgID)
	return err
}

// NewSession выпускает токен сессии для пользователя. Токенов может быть
// несколько (несколько устройств); срока жизни пока нет — прототип.
func (s *Store) NewSession(tgID int64) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := time.Now().Unix()
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

// SaveMessage пишет сообщение в историю канала и возвращает его id.
func (s *Store) SaveMessage(channel string, tgID int64, text string, ts int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO messages (channel, tg_id, text, ts) VALUES (?, ?, ?, ?)`,
		channel, tgID, text, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// History возвращает до limit последних сообщений канала в хронологическом
// порядке (по возрастанию id). beforeID > 0 — страница вверх: только сообщения
// старше него.
func (s *Store) History(channel string, beforeID int64, limit int) ([]MessageData, error) {
	// имя, @username и аватар автора — JOIN из users по tg_id (в messages их
	// нет); LEFT JOIN на случай, если аккаунт автора удалён — тогда пустые.
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

// UserBySession возвращает пользователя по токену сессии (nil — сессии нет)
// и отмечает сессию как живую.
func (s *Store) UserBySession(token string) (*User, error) {
	var u User
	var acceptedAt int64
	err := s.db.QueryRow(`
		SELECT u.tg_id, u.tg_username, u.full_name, u.avatar_url, u.rules_accepted_at
		FROM sessions s JOIN users u ON u.tg_id = s.tg_id
		WHERE s.token = ?`, token).
		Scan(&u.TgID, &u.TgUsername, &u.FullName, &u.AvatarURL, &acceptedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.RulesAccepted = acceptedAt > 0
	now := time.Now().Unix()
	s.db.Exec(`UPDATE sessions SET seen_at = ? WHERE token = ?`, now, token)
	s.db.Exec(`UPDATE users SET seen_at = ? WHERE tg_id = ?`, now, u.TgID)
	return &u, nil
}
