# ether-server

Бэкенд **Ether** — географического IRC, где мир разбит на вложенные
административные каналы (страна → область → город → район → квартал), и
пользователь автоматически состоит сразу в нескольких. Сервер геокодит координаты
в набор **ID каналов**, держит WebSocket-соединения и рассылает сообщения
подписчикам канала.

> ⚠️ **Прототип.** Язык — Go. Общий контекст проекта (идея, модель каналов,
> контракт ID каналов) — в репозитории [`ether-meta`](../ether-meta/CLAUDE.md);
> серверные детали — в [`CLAUDE.md`](./CLAUDE.md).

## Запуск

Конфиг — на окружение: `config.<env>.json` (в git не идёт, образец —
[`config.example.json`](./config.example.json)). Внутри: `addr` (по умолчанию
`:8080`), `telegram_bot_token` (обязателен — без него сервер не стартует; на
каждое окружение свой бот), опциональные `nominatim_url` (пусто → публичный
сервер OSM) и `db` — путь к SQLite-файлу (пусто → `ether.<env>.db`).

```sh
cp config.example.json config.dev.json   # вписать токен бота
go run .                                 # -env dev по умолчанию
go run . -env prod                       # возьмёт config.prod.json
go run . -config /etc/ether/custom.json  # явный путь вместо -env
```

Сервер принимает WebSocket-соединения на `/ws` (`ws://localhost:8080/ws`).

## Как это работает

Клиент шлёт серверу **координаты**, сервер через геокодинг вычисляет набор ID
каналов для точки, подписывает соединение на них и рассылает сообщения. Два
клиента в одном месте получают одинаковые ID → попадают в один чат.

Компоненты:

| Файл | Роль |
|---|---|
| `main.go` | точка входа: флаги `-env`/`-config`, WebSocket-эндпоинт `/ws`, поднятие хаба |
| `config.go` | `Config` — конфиг окружения (`config.<env>.json`) |
| `store.go` | `Store` — персистентность в SQLite: пользователи (tg id), сессии, сообщения каналов |
| `hub.go` | `Hub` — владеет подписками `channelID → клиенты`, рассылает сообщения; всё состояние меняется из одной горутины (без блокировок) |
| `client.go` | `Client` — одно соединение; `readPump` читает кадры, `writePump` — единственный писатель в сокет |
| `geocode.go` | интерфейс `Geocoder` (координаты → каналы), тип `Channel`, `StubGeocoder` для офлайн-прогонов |
| `nominatim.go` | `NominatimGeocoder` — реальный геокодинг через Nominatim (reverse + details, слоты по `rank_address`) |
| `telegram.go` | вход через Telegram: deep-link боту, long-poll `getUpdates` |
| `protocol.go` | wire-протокол: `Envelope` и типы сообщений |

## Wire-протокол

Каждый WebSocket-кадр — это `Envelope`: тег типа + сырой payload (полный контракт
— в [`ether-meta/PROTOCOL.md`](../ether-meta/PROTOCOL.md)).

```json
{ "type": "locate", "data": { "lat": 55.76, "lng": 37.61 } }
```

| Тип | Направление | Payload |
|---|---|---|
| `login_telegram` | client → server | `{}` — запросить ссылку входа |
| `resume` | client → server | `{token}` — восстановить сессию после реконнекта |
| `locate` | client → server | `{lat, lng}` |
| `publish` | client → server | `{channel, text}` — только после `authed` |
| `history` | client → server | `{channel, before_id?, limit?}` — догрузить историю канала |
| `located` | server → client | `{channels: [...]}` |
| `message` | server → client | `{id, channel, sender, text, ts}` |
| `history` | server → client | `{channel, messages: [...]}` — хронологически, по возрастанию `id` |
| `login_link` | server → client | `{url}` — deep-link `t.me/<бот>?start=<токен>` |
| `authed` | server → client | `{user: {id, nick, username}, token}` — `token` сохранить для `resume` |
| `error` | server → client | `{code, message}` |

`Channel` — `{id, level, label, name}`, где `id` — стабильный ключ
(`RU`, `RU-MOW`, `relation/2555133`), `level` ∈ `country | region | city |
district | quarter`.

## Быстрая проверка

С [`websocat`](https://github.com/vi/websocat):

```sh
go run .                                   # в одном терминале
websocat ws://localhost:8080/ws            # в другом
```

Затем в сессии `websocat` по строке:

```json
{"type":"login_telegram","data":{}}
{"type":"locate","data":{"lat":55.76,"lng":37.61}}
{"type":"publish","data":{"channel":"relation/2555133","text":"привет"}}
```

На `login_telegram` сервер вернёт `login_link` — открыть ссылку и нажать Start у
бота, придёт `authed`. После `locate` сервер ответит `located` с набором каналов
(геокодинг через публичный Nominatim занимает ~2–3 с), а `publish` (доступен
только после входа) разошлёт `message` всем подписчикам указанного канала
(включая отправителя).

## Статус

Каркас рабочий: геокодинг (`NominatimGeocoder`), вход через Telegram,
персистентные пользователи, сессии и сообщения (SQLite: `resume` после
реконнекта, история канала кадром `history`), подписка и рассылка сквозь хаб.
Не реализовано:

- **Переподписка при движении** — `locate` сейчас только *добавляет* подписки;
  диффа со снятием со старых уровней нет.
- **Логаут / TTL сессий** — `resume`-токены живут бессрочно, отзыва нет.
- **Свой Nominatim** — публичный сервер ограничен 1 req/s (запросы
  сериализуются), для production нужен свой инстанс (`nominatim_url` в конфиге).

Зависимости: [`gorilla/websocket`](https://github.com/gorilla/websocket). Go 1.26.
