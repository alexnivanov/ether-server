package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// legacyDB — база, какой она была ДО версии 1: схема без колонки client_msg_id,
// user_version нулевой. Ровно то, что сейчас лежит на проде.
func legacyDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// минимум, которого хватает для миграции: таблица, которую она правит
	if _, err := db.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at INTEGER NOT NULL, seen_at INTEGER NOT NULL);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel TEXT NOT NULL,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			text TEXT NOT NULL,
			ts INTEGER NOT NULL);
		INSERT INTO users (created_at, seen_at) VALUES (0, 0);
		INSERT INTO messages (channel, user_id, text, ts) VALUES ('RU', 1, 'до миграции', 1000);
	`); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })
	return path, reopened
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = true
	}
	return out
}

// TestMigrateFreshDB — чистая база собирается из storeSchema целиком, поэтому
// шаги ей не нужны: их надо только проштамповать. Иначе первый же шаг полез бы
// добавлять колонку, которая уже создана схемой.
func TestMigrateFreshDB(t *testing.T) {
	s := openTestStore(t)
	if got := userVersion(t, s.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if cols := columns(t, s.db, "messages"); !cols["client_msg_id"] {
		t.Errorf("в свежей базе нет колонки версии 1: %v", cols)
	}
}

// TestMigrateLegacyDB — база, дожившая с прошлой схемы, догоняется: колонки
// появляются, версия поднимается, данные остаются.
func TestMigrateLegacyDB(t *testing.T) {
	path, db := legacyDB(t)
	if got := userVersion(t, db); got != 0 {
		t.Fatalf("исходная user_version = %d, want 0", got)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if got := userVersion(t, store.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
	if cols := columns(t, store.db, "messages"); !cols["client_msg_id"] {
		t.Errorf("миграция не добавила колонку: %v", cols)
	}
	var text, clientMsgID string
	if err := store.db.QueryRow(
		`SELECT text, client_msg_id FROM messages WHERE id = 1`).Scan(&text, &clientMsgID); err != nil {
		t.Fatalf("старая строка: %v", err)
	}
	if text != "до миграции" {
		t.Errorf("данные потерялись: text = %q", text)
	}
	if clientMsgID != "" {
		t.Errorf("client_msg_id старой строки = %q, want пусто", clientMsgID)
	}
}

// TestMigrateTwice — второй запуск на той же базе не должен делать ничего:
// сервер перезапускается при каждом деплое, и повторное применение шага
// свалило бы старт.
func TestMigrateTwice(t *testing.T) {
	path, _ := legacyDB(t)

	first, err := OpenStore(path)
	if err != nil {
		t.Fatalf("первый запуск: %v", err)
	}
	first.Close()

	second, err := OpenStore(path)
	if err != nil {
		t.Fatalf("второй запуск: %v", err)
	}
	t.Cleanup(func() { second.Close() })
	if got := userVersion(t, second.db); got != schemaVersion {
		t.Errorf("user_version = %d, want %d", got, schemaVersion)
	}
}

// TestMigrateFailingStepKeepsVersion — шаг и повышение версии в одной
// транзакции: упавший шаг не оставляет базу наполовину переехавшей, и следующий
// запуск начнёт его заново.
func TestMigrateFailingStepKeepsVersion(t *testing.T) {
	_, db := legacyDB(t)

	// подменяем список шагов на заведомо ломающийся
	saved := migrations
	migrations = [][]string{
		1: {
			`ALTER TABLE messages ADD COLUMN ok TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE messages ADD COLUMN этого_не_будет INTEGER NOT NULL`, // NOT NULL без DEFAULT
		},
	}
	t.Cleanup(func() { migrations = saved })

	if err := migrate(db, false); err == nil {
		t.Fatal("миграция прошла, а шаг невалидный")
	}
	if got := userVersion(t, db); got != 0 {
		t.Errorf("user_version = %d, want 0 — версия не должна подниматься при сбое", got)
	}
	if cols := columns(t, db, "messages"); cols["ok"] {
		t.Error("первый оператор шага остался применённым — транзакция не откатилась")
	}
}

// TestMigrateNewerDB — база от более новой сборки (откатили бинарник). Старт не
// запрещаем: лишняя колонка старому коду обычно не мешает, а отказ отнял бы
// единственный быстрый способ откатиться.
func TestMigrateNewerDB(t *testing.T) {
	_, db := legacyDB(t)
	if err := setUserVersion(db, schemaVersion+5); err != nil {
		t.Fatalf("штамп версии: %v", err)
	}

	if err := migrate(db, false); err != nil {
		t.Errorf("migrate вернул ошибку %v, а должен пропустить миграции", err)
	}
	if got := userVersion(t, db); got != schemaVersion+5 {
		t.Errorf("user_version = %d, want %d — чужую версию не трогаем", got, schemaVersion+5)
	}
}

// TestMigrationsMatchSchemaVersion — самая частая ошибка при добавлении шага:
// дописать его в migrations и забыть поднять schemaVersion (тогда шаг не
// выполнится) или наоборот (тогда индекс за пределами списка уронит старт).
func TestMigrationsMatchSchemaVersion(t *testing.T) {
	if got := len(migrations) - 1; got != schemaVersion {
		t.Errorf("шагов %d, schemaVersion = %d — они должны совпадать", got, schemaVersion)
	}
	if migrations[0] != nil {
		t.Error("migrations[0] занят: версия 0 — это база без миграций, шага у неё нет")
	}
}

// TestMigrateMissingStep — подняли schemaVersion, а шаг не дописали. Без
// проверки границ это выход за пределы среза, то есть паника внутри OpenStore и
// краш-луп сервиса вместо внятной ошибки старта.
func TestMigrateMissingStep(t *testing.T) {
	_, db := legacyDB(t)

	saved := migrations
	migrations = [][]string{} // шага 1 нет вовсе
	t.Cleanup(func() { migrations = saved })

	err := migrate(db, false)
	if err == nil {
		t.Fatal("ошибки нет, а шаг не описан")
	}
	if !strings.Contains(err.Error(), "не описан") {
		t.Errorf("текст ошибки = %q, хочется внятного про отсутствующий шаг", err)
	}
	if got := userVersion(t, db); got != 0 {
		t.Errorf("user_version = %d, want 0", got)
	}
}
