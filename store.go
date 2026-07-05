package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // чистый Go, без cgo
)

// Store — персистентность в SQLite (файл `db` из конфига, свой на окружение).
// Сейчас здесь пользователи и сессии; хранение сообщений ляжет сюда же.
//
// Пользователь создаётся/обновляется при каждом входе через Telegram (ключ —
// tg id). Сессия — случайный токен, который клиент получает в `authed` и
// предъявляет в `resume` после реконнекта, чтобы не ходить к боту заново.
type Store struct {
	db *sql.DB
}

type User struct {
	TgID      int64
	Username  string
	FirstName string
	Nick      string
}

const storeSchema = `
CREATE TABLE IF NOT EXISTS users (
	tg_id      INTEGER PRIMARY KEY,      -- Telegram user id
	username   TEXT NOT NULL DEFAULT '',
	first_name TEXT NOT NULL DEFAULT '',
	nick       TEXT NOT NULL,
	created_at INTEGER NOT NULL,         -- unix-секунды
	seen_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	tg_id      INTEGER NOT NULL REFERENCES users(tg_id),
	created_at INTEGER NOT NULL,
	seen_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_tg_id ON sessions(tg_id);
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
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SaveUser создаёт или обновляет пользователя (ключ — tg id): username и ник
// подхватываются заново при каждом входе.
func (s *Store) SaveUser(u User) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO users (tg_id, username, first_name, nick, created_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tg_id) DO UPDATE SET
			username = excluded.username,
			first_name = excluded.first_name,
			nick = excluded.nick,
			seen_at = excluded.seen_at`,
		u.TgID, u.Username, u.FirstName, u.Nick, now, now)
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

// UserBySession возвращает пользователя по токену сессии (nil — сессии нет)
// и отмечает сессию как живую.
func (s *Store) UserBySession(token string) (*User, error) {
	var u User
	err := s.db.QueryRow(`
		SELECT u.tg_id, u.username, u.first_name, u.nick
		FROM sessions s JOIN users u ON u.tg_id = s.tg_id
		WHERE s.token = ?`, token).
		Scan(&u.TgID, &u.Username, &u.FirstName, &u.Nick)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	s.db.Exec(`UPDATE sessions SET seen_at = ? WHERE token = ?`, now, token)
	s.db.Exec(`UPDATE users SET seen_at = ? WHERE tg_id = ?`, now, u.TgID)
	return &u, nil
}
