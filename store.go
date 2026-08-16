package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // чистый Go, без cgo
)

// Store — персистентность в SQLite (файл `db` из конфига, свой на окружение):
// пользователи, способы входа, сессии и сообщения каналов.
//
// Личность отвязана от провайдера входа: ключ пользователя — внутренний
// `users.id`, а Telegram/Apple/Google лежат в `identities` (см. схему). Это
// нужно было и для App Store (вход обязан работать без установки Telegram —
// guideline 4.2.3), и для приватности: наружу уходит только внутренний id, а
// Telegram id не покидает базу.
//
// Сессия — случайный токен, который клиент получает при входе и предъявляет в
// `resume` после реконнекта. Сообщение хранится под ключом-строкой ID канала;
// история отдаётся кадром `history` страницами вверх (before_id).
type Store struct {
	db *sql.DB
}

// User — аккаунт для внутреннего использования (хендлеры, WS-соединение).
// Провайдера входа здесь нет специально: после аутентификации он никого не
// интересует, дальше все работают с ID.
type User struct {
	ID int64 // внутренний users.id — единственный идентификатор, уходящий клиенту
	// TgUsername — @username для ссылки на профиль в Telegram. Заполнен только
	// у тех, кто входил через Telegram; у Apple/Google пусто.
	TgUsername string
	FullName   string // отображаемое имя, единственное для UI
	AvatarURL  string // URL фото профиля; у Apple всегда пусто (SIWA его не отдаёт)
	// CreatedAt — когда аккаунт зарегистрирован. Нужен лимитеру: у свежих
	// аккаунтов частота публикаций ниже (см. ratelimit.go) — это делает ферму
	// одноразовых аккаунтов бесполезной независимо от того, насколько дёшево
	// достаётся личность у провайдера.
	CreatedAt time.Time
	// RulesAccepted — согласие с правилами Эфира привязано к аккаунту, а не к
	// устройству/сессии: однажды принял — экран правил больше не увидит, даже
	// переустановив клиент или потеряв shared_preferences.
	RulesAccepted bool
	// Banned — активное наказание модератора: отправка сообщений запрещена
	// («мьют»). Чтение и вход остаются — временный бан никого не выселяет.
	Banned bool
	// BanPermanent — постоянный бан. Аккаунт при этом удалён, и вход запрещён
	// совсем: иначе нарушитель просто зарегистрировался бы заново тем же
	// Apple ID / Telegram.
	BanPermanent bool
}

// Провайдеры входа. Значения попадают в БД (identities.provider, bans.provider),
// поэтому менять их нельзя без миграции.
const (
	ProviderTelegram = "telegram"
	ProviderApple    = "apple"
	ProviderGoogle   = "google"
)

// ProviderUser — результат проверки токена провайдера (см. oidc.go): кто вошёл и
// что провайдер о нём рассказал. Name/AvatarURL могут быть пустыми — Apple
// отдаёт имя только при ПЕРВОЙ авторизации, а фото не отдаёт никогда, поэтому
// пустые значения не должны затирать уже сохранённые (см. UpsertByIdentity).
type ProviderUser struct {
	Provider  string // ProviderTelegram | ProviderApple | ProviderGoogle
	UID       string // id у провайдера: Telegram user id, Apple/Google sub
	Username  string // @username — только Telegram
	Name      string // отображаемое имя
	AvatarURL string // URL фото профиля
}

const storeSchema = `
CREATE TABLE IF NOT EXISTS users (
	id                INTEGER PRIMARY KEY AUTOINCREMENT, -- внутренний id (наружу уходит только он)
	tg_username       TEXT NOT NULL DEFAULT '', -- @username; только у Telegram-аккаунтов
	full_name         TEXT NOT NULL DEFAULT '', -- отображаемое имя (для UI)
	avatar_url        TEXT NOT NULL DEFAULT '', -- URL фото профиля; у Apple пусто
	created_at        INTEGER NOT NULL,    -- unix-миллисекунды (как всюду в БД)
	seen_at           INTEGER NOT NULL,
	rules_accepted_at INTEGER NOT NULL DEFAULT 0 -- unix-мс; 0 — не принял
);
-- Способы входа. У пользователя их может быть несколько: вошёл через Apple,
-- позже привязал Telegram (POST /profile/link/<провайдер>) — аккаунт тот же. provider_uid — строка: у Apple/Google это opaque sub, а не
-- число, и в int64 он не влезает.
CREATE TABLE IF NOT EXISTS identities (
	provider     TEXT NOT NULL,           -- telegram | apple | google
	provider_uid TEXT NOT NULL,           -- id у провайдера (Telegram id, sub)
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (provider, provider_uid)
);
CREATE INDEX IF NOT EXISTS identities_user ON identities(user_id);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	-- ON DELETE CASCADE: удаление аккаунта (DeleteUser) само сносит все его
	-- сессии; работает при foreign_keys=ON (см. OpenStore).
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL, -- unix-миллисекунды
	seen_at    INTEGER NOT NULL  -- unix-миллисекунды
);
CREATE INDEX IF NOT EXISTS sessions_user_id ON sessions(user_id);
CREATE TABLE IF NOT EXISTS messages (
	id      INTEGER PRIMARY KEY AUTOINCREMENT, -- монотонный, курсор пагинации
	channel TEXT NOT NULL,                     -- ID канала (контракт ether-meta)
	-- автор; имя/аватар — JOIN из users. ON DELETE CASCADE: удаление аккаунта
	-- стирает и его сообщения (при foreign_keys=ON).
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
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
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id      INTEGER NOT NULL,        -- id сообщения на момент жалобы
	channel         TEXT NOT NULL,           -- где было сообщение
	author_user_id  INTEGER NOT NULL,        -- на кого жалуются
	message_text    TEXT NOT NULL,           -- копия: сообщение может быть удалено
	reporter_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	reason          TEXT NOT NULL DEFAULT '',-- код причины из клиента
	created_at      INTEGER NOT NULL,        -- unix-миллисекунды
	-- повторная жалоба того же человека на то же сообщение — не новая запись
	UNIQUE(message_id, reporter_user_id)
);
CREATE INDEX IF NOT EXISTS reports_created_at ON reports(created_at);
CREATE INDEX IF NOT EXISTS reports_author ON reports(author_user_id);
-- Адресная доставка пушей (см. push.go). Топики FCM не годятся: в топик нельзя
-- НЕ отправить конкретному подписчику, из-за чего автор получал уведомление о
-- своём же сообщении. Поэтому сервер держит токены устройств и каналы каждого
-- пользователя и шлёт адресно, исключая отправителя.
CREATE TABLE IF NOT EXISTS device_tokens (
	fcm_token  TEXT PRIMARY KEY,        -- токен устройства (он же ключ: при
	                                    -- переустановке/смене аккаунта токен
	                                    -- переезжает к другому пользователю)
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	platform   TEXT NOT NULL DEFAULT '',-- ios|android, для диагностики
	updated_at INTEGER NOT NULL         -- unix-миллисекунды
);
CREATE INDEX IF NOT EXISTS device_tokens_user_id ON device_tokens(user_id);
-- Каналы пользователя: обновляются на каждый locate (сервер и так их вычисляет).
-- Нужны, чтобы понять, кому слать пуш, когда WS-соединения уже нет, и чтобы
-- показать в клиенте размер аудитории канала (ChannelSubscribers).
CREATE TABLE IF NOT EXISTS user_channels (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	channel    TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, channel)
);
CREATE INDEX IF NOT EXISTS user_channels_channel ON user_channels(channel);
-- Блокировка пользователя пользователем (Apple 1.2: «mechanism for users to
-- block abusive users»). Односторонняя: блокирующий перестаёт видеть сообщения
-- заблокированного (в истории — фильтром на сервере, в живой ленте — на
-- клиенте), но не наоборот. Отдельная от bans: там наказание модератора, здесь
-- личное решение человека, и модерацию оно не заменяет.
CREATE TABLE IF NOT EXISTS blocks (
	blocker_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	blocked_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at      INTEGER NOT NULL,
	PRIMARY KEY (blocker_user_id, blocked_user_id)
);
-- под фильтр истории (кого скрывать этому читателю) и под исключение из пушей
CREATE INDEX IF NOT EXISTS blocks_blocked ON blocks(blocked_user_id);
-- Наказания модератора.
--
-- Ключ — ЛИЧНОСТЬ У ПРОВАЙДЕРА, а не users.id, и таблица специально без FK:
-- удаление аккаунта уносит users вместе с identities, а наказание должно это
-- переживать — иначе нарушитель удалит аккаунт и войдёт тем же Apple ID
-- (Telegram id) с чистого листа. Вход проверяется по личности ещё до того, как
-- мы нашли или создали пользователя (см. BanStatusForIdentity).
--
-- user_id — копия на момент наказания, без FK: она нужна модератору (команды в
-- канале принимают внутренний id) и продолжает работать после того, как
-- аккаунта уже нет, — например /unban после /block.
--
-- Автоматической эскалации нет: решает модератор. Он видит count (сколько раз
-- наказывали) в посте жалобы и сам выбирает мьют на сутки или постоянный бан.
CREATE TABLE IF NOT EXISTS bans (
	provider     TEXT NOT NULL,
	provider_uid TEXT NOT NULL,
	user_id      INTEGER NOT NULL DEFAULT 0,   -- users.id на момент наказания (может быть удалён)
	until        INTEGER NOT NULL DEFAULT 0,   -- unix-мс, до когда временный бан; 0 — нет
	permanent    INTEGER NOT NULL DEFAULT 0,   -- 1 — постоянный запрет входа
	count        INTEGER NOT NULL DEFAULT 0,   -- сколько раз наказывали (справочно, для модератора)
	reason       TEXT NOT NULL DEFAULT '',     -- причина последнего наказания (показывается пользователю)
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (provider, provider_uid)
);
CREATE INDEX IF NOT EXISTS bans_user_id ON bans(user_id);
-- Обращения к ссылке установки (GET /app, см. handleInvite в rest.go): кто
-- позвал, откуда взята ссылка и куда мы человека отправили. Основной сценарий —
-- приглашение из приложения, но строка пишется на любой заход, поэтому таблица
-- называется по событию, а не по приглашению (uid опционален).
--
-- Пишется, только когда ссылку открыл БРАУЗЕР. Как подключим
-- apple-app-site-association, у кого приложение уже стоит — ссылка откроется
-- прямо в нём, минуя сервер. То есть таблица считает заходы тех, у кого Эфира
-- нет, а это и есть смысл метрики приглашений.
CREATE TABLE IF NOT EXISTS app_access (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	ts       INTEGER NOT NULL,   -- unix-миллисекунды (как всюду в БД)
	-- пригласивший (users.id из ссылки); NULL — ссылку открыли без приглашения.
	-- Специально без REFERENCES users(id): удаление аккаунта не должно стирать
	-- историю заходов, она про факт перехода, а не про живого пользователя.
	-- После удаления аккаунта uid остаётся осиротевшим числом, и это безопасно:
	-- users.id объявлен AUTOINCREMENT, а такие id SQLite никогда не выдаёт
	-- повторно — значит новый пользователь чужие заходы себе не заберёт.
	uid      INTEGER,
	-- откуда взята ссылка (значения задаёт клиент, приходят как есть):
	--   apli — меню «Пригласить» в приложении
	--   apqr — QR-код
	-- Коды короткие намеренно: ссылка печатается в QR, лишние символы там
	-- поднимают версию кода и делают его плотнее.
	src      TEXT,
	platform TEXT NOT NULL,      -- ios|android|desktop|unknown — ДОГАДКА по User-Agent
	-- outcome — куда фактически отправили: appstore|play|landing. Отдельно от
	-- platform намеренно: platform — это разбор UA, и он меняет смысл задним
	-- числом, когда правишь парсер; outcome — записанное решение сервера, и оно
	-- остаётся верным, что бы мы ни поменяли потом. Разделение уже окупилось:
	-- до публикации в Google Play Android уводили на лендинг, и те строки лежат
	-- как android/landing — не как ошибка, а как решение того времени.
	outcome  TEXT NOT NULL,
	ua       TEXT,               -- сырой User-Agent: platform можно перечитать заново
	lang     TEXT,               -- Accept-Language
	ip       TEXT                -- для определения гео-региона
);
CREATE INDEX IF NOT EXISTS app_access_ts  ON app_access(ts);
CREATE INDEX IF NOT EXISTS app_access_uid ON app_access(uid, ts);
-- Отметки об отправленных еженедельных сводках (см. stats.go). Одна строка на
-- момент расписания (суббота, statsHour), за который сводка ушла в Telegram.
--
-- Это не журнал, а ЗАЩЁЛКА, и она решает две задачи сразу. Сервер проверяет
-- расписание раз в час, а не спит до нужной минуты: пропущенный из-за
-- перезапуска или простоя момент догоняется следующей проверкой, а не теряется
-- (у крона в такой ситуации запуск просто пропадает). При этом PRIMARY KEY по
-- моменту расписания не даёт отправить одну и ту же сводку дважды, сколько бы
-- раз сервер ни перезапускался за неделю.
--
-- Название не weekly_reports: reports рядом — это жалобы на сообщения, и две
-- таблицы с почти одинаковым именем и разным смыслом путали бы.
CREATE TABLE IF NOT EXISTS weekly_stats (
	scheduled_at INTEGER PRIMARY KEY, -- unix-мс момента расписания, за который сводка отправлена
	sent_at      INTEGER NOT NULL     -- unix-мс фактической отправки
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
	// Миграции. Приложение опубликовано в App Store и Google Play, в прод-базе
	// живые аккаунты, сессии и сообщения — пересоздавать её с нуля больше нельзя
	// (раньше было можно, и на этом выкатывали переход на несколько провайдеров
	// входа). А CREATE TABLE IF NOT EXISTS выше существующую таблицу не трогает:
	// новая колонка, дописанная в storeSchema, на живой базе просто не появится —
	// сервер поднимется и начнёт падать на запросах.
	//
	// Поэтому любое изменение схемы = ДВА места: колонка в storeSchema (чтобы
	// чистая база создавалась одним куском) и идемпотентный шаг здесь — ALTER
	// TABLE, глотающий ошибку "duplicate column name", чтобы повторный запуск не
	// падал. Шагов пока нет; первый понадобится под client_msg_id, см.
	// ether-meta/PLANS.md.
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

// ─── пользователи и способы входа ───

// UpsertByIdentity — вход: находит пользователя по личности у провайдера, а если
// такой личности ещё нет — создаёт аккаунт. Возвращает id, признак первого входа
// (created — для приветственного уведомления) и принимал ли этот аккаунт правила.
//
// Имя провайдер задаёт ОДИН раз — при регистрации, пока оно пустое; дальше не
// трогает. Отдельный флаг «имя задано вручную» для этого не нужен: непустое имя
// и есть признак того, что оно уже чьё-то — либо пришло от провайдера при
// регистрации, либо человек ввёл его сам (SetUserName). Иначе тот, кто вводил
// имя на экране онбординга, терял бы его при следующем входе через Telegram.
// Правило то же, что при привязке (LinkIdentity), — одно на оба пути.
//
// Плата за это: сменив имя в Telegram, человек не увидит его в Эфире. Аватар под
// правило не попадает и обновляется с каждым входом — своего аватара у нас
// завести нельзя, так что провайдер тут единственный источник.
//
// Пустой AvatarURL сохранённый не затирает: Apple фото не отдаёт вовсе, и второй
// вход через Apple иначе обнулил бы аватар, подтянутый привязкой Telegram. Telegram-специфичный @username трогаем только когда
// вход шёл через Telegram (там пустое значение — законное: человек снял
// username), при входе через Apple/Google он должен остаться как был.
func (s *Store) UpsertByIdentity(pu ProviderUser) (userID int64, created bool, rulesAccepted bool, err error) {
	now := time.Now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback()

	// @username: NULL — «не трогать», '' — «очистить» (только для Telegram)
	var username any
	if pu.Provider == ProviderTelegram {
		username = pu.Username
	}
	// аватар: NULL — «не трогать» (Apple), иначе значение провайдера
	var avatar any
	if pu.AvatarURL != "" {
		avatar = pu.AvatarURL
	}

	err = tx.QueryRow(`SELECT user_id FROM identities WHERE provider = ? AND provider_uid = ?`,
		pu.Provider, pu.UID).Scan(&userID)
	switch {
	case err == sql.ErrNoRows:
		res, err := tx.Exec(`
			INSERT INTO users (tg_username, full_name, avatar_url, created_at, seen_at)
			VALUES (?, ?, ?, ?, ?)`,
			pu.Username, pu.Name, pu.AvatarURL, now, now)
		if err != nil {
			return 0, false, false, err
		}
		if userID, err = res.LastInsertId(); err != nil {
			return 0, false, false, err
		}
		if _, err := tx.Exec(`
			INSERT INTO identities (provider, provider_uid, user_id, created_at) VALUES (?, ?, ?, ?)`,
			pu.Provider, pu.UID, userID, now); err != nil {
			return 0, false, false, err
		}
		created = true
	case err != nil:
		return 0, false, false, err
	default:
		var acceptedAt int64
		if err := tx.QueryRow(`
			UPDATE users SET
				tg_username = COALESCE(?, tg_username),
				full_name   = CASE WHEN full_name = '' THEN COALESCE(NULLIF(?, ''), full_name) ELSE full_name END,
				avatar_url  = COALESCE(?, avatar_url),
				seen_at     = ?
			WHERE id = ?
			RETURNING rules_accepted_at`,
			username, pu.Name, avatar, now, userID,
		).Scan(&acceptedAt); err != nil {
			return 0, false, false, err
		}
		rulesAccepted = acceptedAt > 0
	}
	return userID, created, rulesAccepted, tx.Commit()
}

// ErrIdentityTaken — эту личность уже занял другой аккаунт. Сливать два аккаунта
// мы не умеем (у каждого свои сообщения, каналы и наказания, и «слияние» — это
// всегда чьи-то потери), поэтому при привязке честнее отказать и объяснить.
var ErrIdentityTaken = errors.New("личность уже привязана к другому аккаунту")

// LinkIdentity привязывает к существующему аккаунту ещё один способ входа.
// Идемпотентна: если эта личность уже привязана к ЭТОМУ аккаунту — не ошибка.
//
// Заодно обогащает профиль тем, что дал провайдер: @username (только Telegram —
// это его атрибут), а имя и аватар — лишь если их не было. Не перезаписываем
// заполненное: человек мог задать имя вручную, и привязка Telegram не повод
// менять его без спроса.
func (s *Store) LinkIdentity(userID int64, pu ProviderUser) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owner int64
	err = tx.QueryRow(`SELECT user_id FROM identities WHERE provider = ? AND provider_uid = ?`,
		pu.Provider, pu.UID).Scan(&owner)
	switch {
	case err == nil && owner != userID:
		return ErrIdentityTaken
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`
			INSERT INTO identities (provider, provider_uid, user_id, created_at) VALUES (?, ?, ?, ?)`,
			pu.Provider, pu.UID, userID, time.Now().UnixMilli()); err != nil {
			return err
		}
	case err != nil:
		return err
	}

	// @username: атрибут Telegram-входа, при привязке именно его и обновляем
	var username any
	if pu.Provider == ProviderTelegram {
		username = pu.Username
	}
	if _, err := tx.Exec(`
		UPDATE users SET
			tg_username = COALESCE(?, tg_username),
			full_name   = CASE WHEN full_name = '' THEN COALESCE(NULLIF(?, ''), full_name) ELSE full_name END,
			avatar_url  = CASE WHEN avatar_url = '' THEN COALESCE(NULLIF(?, ''), avatar_url) ELSE avatar_url END
		WHERE id = ?`,
		username, pu.Name, pu.AvatarURL, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// UserByID — профиль по внутреннему id (нужен после UpsertByIdentity, чтобы
// отдать клиенту итоговые имя/аватар: они могли остаться от прошлого входа).
// nil — пользователя нет.
func (s *Store) UserByID(id int64) (*User, error) {
	var u User
	var acceptedAt int64
	err := s.db.QueryRow(`
		SELECT id, tg_username, full_name, avatar_url, rules_accepted_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.TgUsername, &u.FullName, &u.AvatarURL, &acceptedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.RulesAccepted = acceptedAt > 0
	return &u, nil
}

// Identities — все способы входа пользователя. Нужны модерации: наказание
// пишется на каждую личность, иначе забаненный войдёт другим провайдером.
func (s *Store) Identities(userID int64) ([]ProviderUser, error) {
	rows, err := s.db.Query(`SELECT provider, provider_uid FROM identities WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderUser
	for rows.Next() {
		var p ProviderUser
		if err := rows.Scan(&p.Provider, &p.UID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetUserName задаёт отображаемое имя вручную. Нужно, когда провайдер имени не
// дал: Apple отдаёт его только при первой авторизации и вне токена, и если тот
// вход не доехал (сеть, ошибка сервера) — взять имя больше негде, а безымянный
// аккаунт в чате бесполезен. Пустое имя не принимаем: это работа вызывающего
// (см. handleSetName).
func (s *Store) SetUserName(userID int64, name string) error {
	_, err := s.db.Exec(`UPDATE users SET full_name = ? WHERE id = ?`, name, userID)
	return err
}

// AcceptRules отмечает, что пользователь принял правила Эфира — привязано к
// аккаунту, переживает переустановку клиента и смену устройства.
func (s *Store) AcceptRules(userID int64) error {
	_, err := s.db.Exec(`UPDATE users SET rules_accepted_at = ? WHERE id = ?`, time.Now().UnixMilli(), userID)
	return err
}

// NewSession выпускает токен сессии для пользователя. Токенов может быть
// несколько (несколько устройств); срока жизни пока нет — прототип.
func (s *Store) NewSession(userID int64) (string, error) {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`INSERT INTO sessions (token, user_id, created_at, seen_at) VALUES (?, ?, ?, ?)`,
		token, userID, now, now)
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
// (ON DELETE CASCADE, см. схему) его способы входа, все сессии (все устройства),
// сообщения, токены устройств и каналы. Идемпотентна: удаление отсутствующего
// id — не ошибка. Записи в bans остаются: наказание живёт по личности у
// провайдера и переживает удаление аккаунта. Строки app_access тоже остаются
// (см. схему): заход по ссылке — факт из прошлого, а осиротевший uid сопоставить
// уже не с чем.
func (s *Store) DeleteUser(userID int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, userID)
	return err
}

// UserBySession возвращает пользователя по токену сессии (nil — сессии нет)
// и отмечает сессию как живую.
func (s *Store) UserBySession(token string) (*User, error) {
	var u User
	var acceptedAt, banUntil int64
	var banPermanent int
	// Наказание живёт по личности (см. схему bans), поэтому подтягиваем его
	// через identities. MAX: у пользователя может быть несколько способов входа,
	// и хватает наказания на любом из них — иначе бан обходился бы вторым
	// провайдером. Временное истекает само: сравниваем until с текущим временем
	// здесь же, фоновая уборка не нужна.
	var createdAt int64
	err := s.db.QueryRow(`
		SELECT u.id, u.tg_username, u.full_name, u.avatar_url, u.rules_accepted_at, u.created_at,
		       COALESCE(MAX(b.until), 0), COALESCE(MAX(b.permanent), 0)
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		LEFT JOIN identities i ON i.user_id = u.id
		LEFT JOIN bans b ON b.provider = i.provider AND b.provider_uid = i.provider_uid
		WHERE s.token = ?
		GROUP BY u.id`, token).
		Scan(&u.ID, &u.TgUsername, &u.FullName, &u.AvatarURL, &acceptedAt, &createdAt,
			&banUntil, &banPermanent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.RulesAccepted = acceptedAt > 0
	u.CreatedAt = time.UnixMilli(createdAt)
	u.BanPermanent = banPermanent == 1
	u.Banned = u.BanPermanent || banUntil > time.Now().UnixMilli()
	now := time.Now().UnixMilli()
	s.db.Exec(`UPDATE sessions SET seen_at = ? WHERE token = ?`, now, token)
	s.db.Exec(`UPDATE users SET seen_at = ? WHERE id = ?`, now, u.ID)
	return &u, nil
}

// ─── блокировки (пользователь → пользователя) ───

// BlockUser — блокирующий больше не видит сообщения заблокированного.
// Идемпотентна. Себя заблокировать нельзя: молча игнорируем, чтобы клиент не
// пришлось учить особому случаю.
func (s *Store) BlockUser(blockerID, blockedID int64) error {
	if blockerID == blockedID {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO blocks (blocker_user_id, blocked_user_id, created_at)
		VALUES (?, ?, ?)`, blockerID, blockedID, time.Now().UnixMilli())
	return err
}

// UnblockUser снимает блокировку. Идемпотентна.
func (s *Store) UnblockUser(blockerID, blockedID int64) error {
	_, err := s.db.Exec(`
		DELETE FROM blocks WHERE blocker_user_id = ? AND blocked_user_id = ?`,
		blockerID, blockedID)
	return err
}

// BlockedUser — заблокированный для показа в списке: id нужен, чтобы снять
// блокировку, остальное — чтобы человек узнал, кого именно он скрыл.
type BlockedUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name,omitempty"`
	Username  string `json:"username,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// BlockedUsers — заблокированные с профилями, для экрана «Заблокированные».
// Отдельно от BlockedBy: там нужны только id (фильтр живой ленты, и он
// приезжает с каждым authed), а здесь профили — и запрашиваются они лишь когда
// человек открыл список.
//
// Новые сверху: если кого-то заблокировали только что и хотят отменить, он
// первый в списке.
func (s *Store) BlockedUsers(userID int64) ([]BlockedUser, error) {
	rows, err := s.db.Query(`
		SELECT u.id, u.full_name, u.tg_username, u.avatar_url
		FROM blocks b JOIN users u ON u.id = b.blocked_user_id
		WHERE b.blocker_user_id = ?
		ORDER BY b.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BlockedUser{}
	for rows.Next() {
		var u BlockedUser
		if err := rows.Scan(&u.ID, &u.Name, &u.Username, &u.AvatarURL); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// BlockedBy — кого заблокировал этот пользователь. Клиент получает список при
// входе и прячет их сообщения в живой ленте (историю фильтрует сервер).
func (s *Store) BlockedBy(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT blocked_user_id FROM blocks WHERE blocker_user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ─── сообщения ───

// SaveMessage пишет сообщение в историю канала и возвращает его id.
func (s *Store) SaveMessage(channel string, userID int64, text string, ts int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO messages (channel, user_id, text, ts) VALUES (?, ?, ?, ?)`,
		channel, userID, text, ts)
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
// viewerID > 0 — читатель известен (клиент прислал токен), и из выборки
// исключаются авторы, которых он заблокировал: иначе после блокировки чужие
// сообщения возвращались бы при каждой подгрузке истории. 0 — анонимное чтение,
// фильтровать нечего.
func (s *Store) History(channel string, beforeID int64, limit int, viewerID int64) ([]MessageData, error) {
	// имя, @username и аватар автора — JOIN из users по user_id (в messages их
	// нет). LEFT JOIN — защитно: при удалении аккаунта сообщения удаляются
	// каскадом вместе с автором (см. DeleteUser), поэтому «висячих» строк без
	// users быть не должно, но JOIN не должен ронять выборку, если что.
	q := `SELECT m.id, m.channel, m.user_id, COALESCE(u.full_name, ''), COALESCE(u.tg_username, ''), COALESCE(u.avatar_url, ''), m.text, m.ts
		FROM messages m LEFT JOIN users u ON u.id = m.user_id
		WHERE m.channel = ?`
	args := []any{channel}
	if viewerID > 0 {
		q += ` AND m.user_id NOT IN (SELECT blocked_user_id FROM blocks WHERE blocker_user_id = ?)`
		args = append(args, viewerID)
	}
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
func (s *Store) DeleteUserMessages(userID int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ─── жалобы ───

// ReportedMessage — что именно нажаловали: данные для разбора и для уведомления
// в служебный Telegram-канал (см. notify.go).
type ReportedMessage struct {
	MessageID      int64
	Channel        string
	AuthorID       int64  // внутренний id автора — им же оперируют команды модерации
	AuthorName     string // имя автора на момент жалобы (JOIN из users)
	AuthorUsername string // @username автора; пусто — нет username или вход не через Telegram
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
func (s *Store) ReportMessage(messageID, reporterID int64, reason string) (*ReportedMessage, error) {
	rep := &ReportedMessage{MessageID: messageID, Reason: reason}
	// LEFT JOIN: имя автора — удобство для разбора, его отсутствие не должно
	// мешать принять жалобу
	err := s.db.QueryRow(`
		SELECT m.channel, m.user_id, m.text, COALESCE(u.full_name, ''), COALESCE(u.tg_username, '')
		FROM messages m LEFT JOIN users u ON u.id = m.user_id
		WHERE m.id = ?`, messageID).
		Scan(&rep.Channel, &rep.AuthorID, &rep.Text, &rep.AuthorName, &rep.AuthorUsername)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO reports
			(message_id, channel, author_user_id, message_text, reporter_user_id, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		messageID, rep.Channel, rep.AuthorID, rep.Text, reporterID, reason, time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	if rep.AuthorBanCount, err = s.BanCount(rep.AuthorID); err != nil {
		return nil, err
	}
	// 0 изменённых строк — сработал IGNORE, т.е. жалоба уже была
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		rep.Fresh = true
	}
	return rep, nil
}

// ─── наказания ───

// banDuration — срок мьюта по умолчанию. Сутки: достаточно, чтобы остудить, и не
// требует от модератора возвращаться и снимать вручную — истекает сам, без
// фоновой уборки (проверка идёт по времени, см. BanStatus).
const banDuration = 24 * time.Hour

// BanTemporary закрывает отправку на banDuration («мьют»): вход и чтение
// остаются, сессии не отзываются. reason — необязательная причина, её увидит
// пользователь. Возвращает время окончания и накопленное число наказаний, чтобы
// модератор видел, сколько раз этот человек уже попадался.
func (s *Store) BanTemporary(userID int64, reason string) (until time.Time, count int, err error) {
	now := time.Now()
	until = now.Add(banDuration)
	if err = s.upsertBan(userID, until.UnixMilli(), 0, reason, now); err != nil {
		return time.Time{}, 0, err
	}
	count, err = s.BanCount(userID)
	return until, count, err
}

// BanPermanent закрывает вход навсегда и удаляет аккаунт (профиль, сессии,
// сообщения и токены устройств уходят каскадом). Записи в bans остаются —
// именно они не дают зарегистрироваться заново тем же Apple ID / Telegram.
// Автоматически не вызывается: решение всегда за модератором.
func (s *Store) BanPermanent(userID int64, reason string) (count int, err error) {
	now := time.Now()
	if err = s.upsertBan(userID, 0, 1, reason, now); err != nil {
		return 0, err
	}
	// счётчик читаем ДО удаления: после него identities уже нет, и наказание
	// находится только по user_id
	if count, err = s.BanCount(userID); err != nil {
		return 0, err
	}
	if err = s.DeleteUser(userID); err != nil {
		return 0, err
	}
	return count, nil
}

// ErrNoIdentities — наказывать некого: у пользователя не осталось способов
// входа, то есть аккаунт уже удалён. Отдельная ошибка, потому что модератору
// нужен внятный ответ в канале, а не «смотри логи».
var ErrNoIdentities = errors.New("у пользователя нет способов входа")

// upsertBan пишет наказание на КАЖДЫЙ способ входа пользователя и увеличивает
// счётчик нарушений. На каждый, потому что иначе забаненный войдёт другим
// провайдером и получит чистый аккаунт.
//
// Если способов входа не осталось (аккаунт уже удалён), наказание не пишется:
// привязать его не к чему — по внутреннему id вернувшегося не опознать, он
// получит новый.
func (s *Store) upsertBan(userID, until int64, permanent int, reason string, now time.Time) error {
	ids, err := s.Identities(userID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return ErrNoIdentities
	}
	nowMs := now.UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`
			INSERT INTO bans (provider, provider_uid, user_id, until, permanent, count, reason, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)
			ON CONFLICT(provider, provider_uid) DO UPDATE SET
				user_id = excluded.user_id,
				until = excluded.until,
				permanent = excluded.permanent,
				count = bans.count + 1,
				reason = excluded.reason,
				updated_at = excluded.updated_at`,
			id.Provider, id.UID, userID, until, permanent, reason, nowMs, nowMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BanCount — сколько раз этого человека наказывали (включая снятые). Модератор
// видит это число в посте жалобы и решает, мьютить или банить насовсем.
//
// Ищем и по личностям, и по user_id — оба варианта нужны:
//   - по личностям, потому что после /block аккаунт удаляется, а вернувшийся
//     регистрируется заново и получает НОВЫЙ внутренний id; счётчик обязан
//     подтянуться к нему, иначе рецидивист выглядит новичком;
//   - по user_id, потому что после удаления аккаунта личностей уже нет, и
//     это единственная зацепка (её же использует Unban).
//
// MAX: наказания пишутся на все личности сразу, значения совпадают.
func (s *Store) BanCount(userID int64) (int, error) {
	var n sql.NullInt64
	err := s.db.QueryRow(`
		SELECT MAX(count) FROM bans
		WHERE user_id = ?
		   OR (provider, provider_uid) IN (SELECT provider, provider_uid FROM identities WHERE user_id = ?)`,
		userID, userID).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return int(n.Int64), err
}

// BanStatusForIdentity — активно ли наказание у конкретной личности провайдера.
// Проверяется при входе, ДО того как мы нашли или создали пользователя: именно
// так постоянный бан переживает удаление аккаунта.
func (s *Store) BanStatusForIdentity(provider, uid string) (banned bool, until time.Time, permanent bool, reason string, err error) {
	var untilMs int64
	var perm int
	err = s.db.QueryRow(`SELECT until, permanent, reason FROM bans WHERE provider = ? AND provider_uid = ?`,
		provider, uid).Scan(&untilMs, &perm, &reason)
	if err == sql.ErrNoRows {
		return false, time.Time{}, false, "", nil
	}
	if err != nil {
		return false, time.Time{}, false, "", err
	}
	return banActive(untilMs, perm, reason)
}

// BanStatus — активно ли наказание у пользователя (по всем его способам входа:
// хватает наказания на любом). Временный бан истекает сам — сравниваем until с
// текущим временем, поэтому фоновая уборка не нужна.
func (s *Store) BanStatus(userID int64) (banned bool, until time.Time, permanent bool, reason string, err error) {
	var untilMs, perm sql.NullInt64
	var reasonNull sql.NullString
	// Строк может не быть вовсе — тогда агрегаты придут NULL, а не ErrNoRows.
	// reason берём у самого свежего наказания (ORDER BY updated_at внутри), но
	// при одном пользователе они и так пишутся одинаковыми.
	err = s.db.QueryRow(`
		SELECT MAX(b.until), MAX(b.permanent),
		       (SELECT b2.reason FROM bans b2 JOIN identities i2 ON i2.provider = b2.provider AND i2.provider_uid = b2.provider_uid
		        WHERE i2.user_id = ? ORDER BY b2.updated_at DESC LIMIT 1)
		FROM bans b JOIN identities i ON i.provider = b.provider AND i.provider_uid = b.provider_uid
		WHERE i.user_id = ?`, userID, userID).Scan(&untilMs, &perm, &reasonNull)
	if err == sql.ErrNoRows {
		return false, time.Time{}, false, "", nil
	}
	if err != nil {
		return false, time.Time{}, false, "", err
	}
	return banActive(untilMs.Int64, int(perm.Int64), reasonNull.String)
}

// banActive — общее правило: permanent игнорирует until, временный активен пока
// until в будущем. Одно место, чтобы толкования не разъезжались.
func banActive(untilMs int64, permanent int, reason string) (bool, time.Time, bool, string, error) {
	if permanent == 1 {
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
//
// Ищет и по user_id, и по личностям — по тем же причинам, что BanCount: после
// /block аккаунта уже нет (остаётся только user_id в самой записи), а после
// повторной регистрации у человека новый внутренний id (связь остаётся только
// через личность).
func (s *Store) Unban(userID int64) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE bans SET until = 0, permanent = 0, updated_at = ?
		WHERE (until > 0 OR permanent = 1)
		  AND (user_id = ?
		       OR (provider, provider_uid) IN (SELECT provider, provider_uid FROM identities WHERE user_id = ?))`,
		time.Now().UnixMilli(), userID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ─── пуши и каналы ───

// SaveDeviceToken привязывает FCM-токен устройства к пользователю (upsert по
// токену). Ключ — сам токен: при переустановке или входе другим аккаунтом FCM
// отдаёт тот же токен, и он должен «переехать» к новому пользователю, а не
// задвоиться.
func (s *Store) SaveDeviceToken(userID int64, fcmToken, platform string) error {
	_, err := s.db.Exec(`
		INSERT INTO device_tokens (fcm_token, user_id, platform, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(fcm_token) DO UPDATE SET
			user_id = excluded.user_id,
			platform = excluded.platform,
			updated_at = excluded.updated_at`,
		fcmToken, userID, platform, time.Now().UnixMilli())
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
func (s *Store) SetUserChannels(userID int64, channels []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM user_channels WHERE user_id = ?`, userID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, ch := range channels {
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO user_channels (user_id, channel, updated_at) VALUES (?, ?, ?)`,
			userID, ch, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChannelSubscribers — сколько пользователей привязано к каждому из каналов
// (по user_channels, которые обновляются на каждый locate). Один запрос на весь
// набор, а не по каналу: locate отдаёт 5–6 каналов, и N+1 здесь ни к чему.
//
// В карте могут отсутствовать каналы без подписчиков — вызывающий трактует
// отсутствие как 0.
func (s *Store) ChannelSubscribers(channels []string) (map[string]int, error) {
	if len(channels) == 0 {
		return map[string]int{}, nil
	}
	q := `SELECT channel, COUNT(*) FROM user_channels WHERE channel IN (?` +
		strings.Repeat(`, ?`, len(channels)-1) + `) GROUP BY channel`
	args := make([]any, len(channels))
	for i, ch := range channels {
		args[i] = ch
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int, len(channels))
	for rows.Next() {
		var ch string
		var n int
		if err := rows.Scan(&ch, &n); err != nil {
			return nil, err
		}
		out[ch] = n
	}
	return out, rows.Err()
}

// PushTargets отдаёт токены устройств, которым надо доставить пуш о новом
// сообщении в канале: все устройства пользователей, у кого этот канал в наборе,
// **кроме** автора сообщения (exceptUserID) — иначе человек получает уведомление
// о своём же сообщении, ради чего адресная модель и вводилась.
func (s *Store) PushTargets(channel string, exceptUserID int64) ([]string, error) {
	// Заблокировавшие автора исключаются вместе с самим автором: обещание «я его
	// больше не вижу» иначе ломалось бы уведомлением на заблокированное сообщение.
	rows, err := s.db.Query(`
		SELECT d.fcm_token
		FROM user_channels uc JOIN device_tokens d ON d.user_id = uc.user_id
		WHERE uc.channel = ? AND uc.user_id != ?
		  AND uc.user_id NOT IN (SELECT blocker_user_id FROM blocks WHERE blocked_user_id = ?)`,
		channel, exceptUserID, exceptUserID)
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

// ─── обращения к ссылке установки ───

// AppAccess — один заход на /app (см. схему app_access и handleInvite).
// UID = 0 значит «без приглашающего» и пишется в базу как NULL: 0 не бывает
// валидным users.id, а NULL честнее говорит «неизвестно» в отчётах.
type AppAccess struct {
	UID      int64
	Src      string
	Platform string
	Outcome  string
	UA       string
	Lang     string
	IP       string
}

// SaveAppAccess пишет заход на ссылку установки. Зовётся из хендлера редиректа,
// и его ошибка не должна мешать редиректу — вызывающий только логирует её.
func (s *Store) SaveAppAccess(a AppAccess) error {
	var uid any
	if a.UID > 0 {
		uid = a.UID
	}
	_, err := s.db.Exec(`
		INSERT INTO app_access (ts, uid, src, platform, outcome, ua, lang, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), uid, a.Src, a.Platform, a.Outcome, a.UA, a.Lang, a.IP)
	return err
}

// ─── еженедельная сводка ───

// CountRow — «что-то и сколько его»: строка любой группировки в сводке.
type CountRow struct {
	Key   string
	Count int
}

// AccessRow — один заход по ссылке установки для построчного списка в сводке.
// InviterName пустое, если позвавший не указан или его аккаунт уже удалён
// (app_access специально без FK на users, см. схему).
type AccessRow struct {
	TS          int64
	UID         int64 // 0 — без приглашающего
	InviterName string
	Src         string
	Platform    string
	Outcome     string
	Lang        string
	UA          string
}

// WeeklyStats — всё, что попадает в еженедельную сводку.
type WeeklyStats struct {
	NewUsers   int
	TotalUsers int
	Messages   int
	// Channels — сколько РАЗНЫХ каналов получило хотя бы одно сообщение.
	// Названий каналов сервер не хранит (в messages лежит ID вида
	// relation/2555133, а имена живут только в памяти геокодера), поэтому топ
	// каналов в сводке не выводим: список сырых ID нечитаем.
	Channels   int
	Accesses   int
	BySrc      []CountRow
	ByPlatform []CountRow
	ByInviter  []CountRow
	AccessRows []AccessRow
}

// WeeklyStats собирает сводку за [from, to). Границы полуоткрытые: соседние
// недели не пересекаются и ни одна строка не попадает в две сводки сразу.
func (s *Store) WeeklyStats(from, to int64) (*WeeklyStats, error) {
	st := &WeeklyStats{}

	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE created_at >= ? AND created_at < ?`,
		from, to).Scan(&st.NewUsers); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&st.TotalUsers); err != nil {
		return nil, err
	}
	// Сообщения живут messageTTL (неделя) и удаляются уборщиком, а окно сводки —
	// ровно неделя: то, что отправлено в самом начале периода, к субботе может
	// уже исчезнуть. Поэтому число сообщений — нижняя оценка, а не точный счёт.
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COUNT(DISTINCT channel) FROM messages WHERE ts >= ? AND ts < ?`,
		from, to).Scan(&st.Messages, &st.Channels); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM app_access WHERE ts >= ? AND ts < ?`,
		from, to).Scan(&st.Accesses); err != nil {
		return nil, err
	}

	var err error
	// COALESCE: src не заполнен у прямых заходов на /app, и в сводке они должны
	// быть видимой строкой, а не пропасть из группировки.
	if st.BySrc, err = s.countRows(`
		SELECT COALESCE(NULLIF(src, ''), 'без источника'), COUNT(*) n
		FROM app_access WHERE ts >= ? AND ts < ?
		GROUP BY 1 ORDER BY n DESC`, from, to); err != nil {
		return nil, err
	}
	if st.ByPlatform, err = s.countRows(`
		SELECT platform, COUNT(*) n
		FROM app_access WHERE ts >= ? AND ts < ?
		GROUP BY 1 ORDER BY n DESC`, from, to); err != nil {
		return nil, err
	}
	// Только заходы с приглашающим: строки без uid в «кто позвал» не отвечают.
	// LEFT JOIN — аккаунт мог быть удалён уже после перехода.
	if st.ByInviter, err = s.countRows(`
		SELECT COALESCE(NULLIF(u.full_name, ''), 'id ' || a.uid), COUNT(*) n
		FROM app_access a LEFT JOIN users u ON u.id = a.uid
		WHERE a.ts >= ? AND a.ts < ? AND a.uid IS NOT NULL
		GROUP BY a.uid ORDER BY n DESC`, from, to); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT a.ts, COALESCE(a.uid, 0), COALESCE(u.full_name, ''),
		       COALESCE(a.src, ''), a.platform, a.outcome,
		       COALESCE(a.lang, ''), COALESCE(a.ua, '')
		FROM app_access a LEFT JOIN users u ON u.id = a.uid
		WHERE a.ts >= ? AND a.ts < ?
		ORDER BY a.ts`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r AccessRow
		if err := rows.Scan(&r.TS, &r.UID, &r.InviterName, &r.Src,
			&r.Platform, &r.Outcome, &r.Lang, &r.UA); err != nil {
			return nil, err
		}
		st.AccessRows = append(st.AccessRows, r)
	}
	return st, rows.Err()
}

// countRows — общий разбор группировок «ключ, количество» для сводки.
func (s *Store) countRows(query string, args ...any) ([]CountRow, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CountRow
	for rows.Next() {
		var r CountRow
		if err := rows.Scan(&r.Key, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WeeklyStatsSent — уходила ли уже сводка за этот момент расписания.
func (s *Store) WeeklyStatsSent(scheduledAt int64) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM weekly_stats WHERE scheduled_at = ?`, scheduledAt).Scan(&n)
	return n > 0, err
}

// MarkWeeklyStatsSent запоминает, что сводка за этот момент расписания
// отправлена. Зовётся ТОЛЬКО после успешной отправки: иначе сбой сети навсегда
// съел бы недельный отчёт (в обратном порядке — Telegram недоступен, а отметка
// уже стоит). INSERT OR IGNORE делает повтор безобидным.
func (s *Store) MarkWeeklyStatsSent(scheduledAt int64) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO weekly_stats (scheduled_at, sent_at) VALUES (?, ?)`,
		scheduledAt, time.Now().UnixMilli())
	return err
}
