#!/usr/bin/env bash
# Резервная копия базы Эфира. Запускается systemd-таймером ether-backup.timer
# (ежедневно) или руками: sudo systemctl start ether-backup.service
#
# Храним ТОЛЬКО последнюю копию: база маленькая, а история версий пока не нужна.
# Копия пишется во временный файл и переносится на место одним `mv` — так на
# диске никогда не лежит недописанный «бэкап», который выглядит как рабочий.
#
# Почему не `cp` самой базы: она в WAL-режиме, и простое копирование файла при
# активной записи даёт битый снимок. `VACUUM INTO` делает согласованную копию,
# не останавливая сервер, и попутно выкидывает свободные страницы.
#
# Переменные окружения (задаются в юните):
#   DB                  путь к базе (по умолчанию /var/lib/ether/ether.prod.db)
#   BACKUP              куда писать копию (по умолчанию /var/lib/ether/backups/ether-latest.db.gz)
#   CONFIG              конфиг сервера — из него берутся токен и чат для алертов
#   MIN_FREE_PCT        порог свободного места для предупреждения (по умолчанию 15)
#   SEND_TO_TELEGRAM=1  выгружать копию в служебный Telegram-канал
#   ENV_FILE            файл с настройками выгрузки (по умолчанию
#                       /etc/ether/backup.env, режим 0600, владелец root)
#
# ВАЖНО про SEND_TO_TELEGRAM: копия содержит персональные данные (Telegram id,
# имена, сообщения), то есть выгрузка — это передача их в Telegram. Канал
# приватный, но решение осознанное: по умолчанию выключено.
#
# ── Выгрузка за пределы сервера ──
#
# Локальная копия лежит на том же диске, что и база: потеря сервера уносит обе.
# Поэтому копия дополнительно уезжает в удалённое хранилище через `rclone` —
# какое именно, скрипт не знает: адрес берётся из BACKUP_REMOTE, а сам remote
# описывается переменными RCLONE_CONFIG_* в $ENV_FILE. Так переезд между
# провайдерами — правка конфига, а не кода (Яндекс.Диск по WebDAV, любое
# S3-совместимое хранилище, Google Drive — всё одинаково).
#
# Перед отправкой копия ШИФРУЕТСЯ через age на ПУБЛИЧНЫЙ ключ: на сервере лежит
# только он, закрытый — в менеджере паролей. Смысл именно в этом: кто получил
# доступ к серверу, не может прочитать ни новые копии, ни старые, а хранилище
# видит лишь шифротекст. Без ключа скрипт не выгружает вовсе — молчаливая
# отправка персональных данных в открытом виде хуже отсутствия выгрузки.
#
# Вместе с базой уезжает и конфиг (`ether-config-*.tar.age`): client id
# провайдеров, токен бота модерации, service-account FCM, DSN Sentry. Без него
# восстановленная база не поднимется, а собрать всё заново по консолям — час
# работы в худший для этого момент. В локальную (нешифрованную) копию секреты не
# попадают.
#
# Заливается два ключа: `ether-latest.db.gz.age` (перезаписывается) и
# `ether-YYYY-MM-DD.db.gz.age` (история). История нужна на случай, когда база
# повредилась, а бэкап исправно её скопировал: «последняя копия» тогда уже
# бесполезна. Срок хранения задаётся правилом жизненного цикла бакета в панели
# Cloudflare (например, удалять объекты старше 30 дней) — скрипт старое не чистит.

set -euo pipefail

# Настройки выгрузки — отдельным файлом, а не в конфиге сервера: у Go-сервера
# нет причин знать ключи хранилища, а у скрипта — токен бота (он и так берётся
# из конфига только для алертов).
ENV_FILE="${ENV_FILE:-/etc/ether/backup.env}"
if [[ -r "$ENV_FILE" ]]; then
	# shellcheck disable=SC1090
	set -a && . "$ENV_FILE" && set +a
fi

DB="${DB:-/var/lib/ether/ether.prod.db}"
BACKUP="${BACKUP:-/var/lib/ether/backups/ether-latest.db.gz}"
CONFIG="${CONFIG:-/etc/ether/config.prod.yaml}"
MIN_FREE_PCT="${MIN_FREE_PCT:-15}"

# ── алерты в служебный Telegram-канал (тот же, что у модерации) ──
# Токен и chat_id читаем из конфига сервера, чтобы не дублировать секреты.
# Конфиг — YAML, но нам нужны две плоские строки, поэтому хватает sed: тащить
# PyYAML/yq на сервер ради этого незачем. Снимаем кавычки, если они есть.
yaml_value() {
	sed -n "s/^$1:[[:space:]]*//p" "$CONFIG" 2>/dev/null |
		head -1 |
		sed -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}
tg_token=""
tg_chat=""
if [[ -r "$CONFIG" ]]; then
	tg_token=$(yaml_value telegram_notify_token || true)
	tg_chat=$(yaml_value telegram_notify_chat_id || true)
fi

alert() {
	echo "$1" >&2
	if [[ -n "$tg_token" && -n "$tg_chat" ]]; then
		curl -sS -m 15 -o /dev/null \
			-X POST "https://api.telegram.org/bot${tg_token}/sendMessage" \
			--data-urlencode "chat_id=${tg_chat}" \
			--data-urlencode "text=🗄 Бэкап Эфира: $1" || true
	fi
}

# Молча ломающийся бэкап — худшее из возможного: кажется, что копия есть, а её
# нет. Поэтому любой сбой уходит в канал.
trap 'alert "ОШИБКА на строке $LINENO — смотри journalctl -u ether-backup"' ERR

[[ -f "$DB" ]] || { alert "база не найдена: $DB"; exit 1; }
mkdir -p "$(dirname "$BACKUP")"

# Временный КАТАЛОГ, а не файл: `VACUUM INTO` отказывается писать в
# существующий файл, а mktemp его как раз создаёт. Каталог решает это без гонки
# (`mktemp -u` вернул бы имя, которое кто-то может занять между проверкой и
# записью).
tmp_dir=$(mktemp -d "$(dirname "$BACKUP")/.backup.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT
tmp_db="$tmp_dir/ether.db"

# busy_timeout: сервер — единственный писатель, но на момент снимка может
# держать транзакцию; ждём, а не падаем.
sqlite3 "$DB" ".timeout 10000" "VACUUM INTO '$tmp_db'"
gzip -9 "$tmp_db"
mv -f "$tmp_db.gz" "$BACKUP" # атомарная замена прошлой копии

size=$(du -h "$BACKUP" | cut -f1)

# ── свободное место: кончившийся диск означает, что сервер вообще не пишет ──
# `df -P` — POSIX-формат, пятая колонка = занято в процентах. Не `--output=pcent`:
# это расширение GNU, и на macOS (где скрипт удобно прогонять локально) его нет.
used_pct=$(df -P "$(dirname "$BACKUP")" | awk 'NR==2 {print $5}' | tr -dc '0-9')
free_pct=$((100 - used_pct))
if ((free_pct < MIN_FREE_PCT)); then
	alert "мало места на диске: свободно ${free_pct}%"
fi

# ── выгрузка копии за пределы сервера (опционально) ──
if [[ "${SEND_TO_TELEGRAM:-0}" == "1" && -n "$tg_token" && -n "$tg_chat" ]]; then
	if curl -sS -m 120 -o /dev/null \
		-F "chat_id=${tg_chat}" \
		-F "document=@${BACKUP}" \
		-F "caption=Бэкап Эфира $(date '+%d.%m %H:%M') (${size})" \
		"https://api.telegram.org/bot${tg_token}/sendDocument"; then
		echo "копия выгружена в Telegram"
	else
		alert "не удалось выгрузить копию в Telegram (на сервере копия сохранена)"
	fi
fi

# ── выгрузка в Cloudflare R2 (шифрованная) ──
# Условие на все переменные разом: половина настроек — это не «частично
# работает», а невыгруженный бэкап, о котором никто не узнает.
if [[ -n "${BACKUP_REMOTE:-}" ]]; then
	if [[ -z "${AGE_RECIPIENT:-}" ]]; then
		alert "хранилище настроено, но нет AGE_RECIPIENT — выгрузка отменена, копию без шифрования не отправляем"
	elif ! command -v age >/dev/null || ! command -v rclone >/dev/null; then
		alert "нет age или rclone — выгрузка невозможна (apt install age rclone)"
	else
		enc="$tmp_dir/ether-latest.db.gz.age"
		age -r "$AGE_RECIPIENT" -o "$enc" "$BACKUP"

		# Сам remote описан переменными RCLONE_CONFIG_* в $ENV_FILE, и они уже
		# экспортированы (`set -a` при чтении файла) — конфиг-файла rclone с
		# ключами на сервере нет вовсе.

		# ── конфиг рядом с базой ──
		# База без конфига не поднимается: нужны client id провайдеров, токен
		# бота модерации, service-account FCM, DSN Sentry. Всё это регенерируется
		# по консолям, но это час кликанья в самый неудачный момент — а весит
		# несколько килобайт. Шифруется тем же ключом; в локальную копию (она без
		# шифрования) секреты НЕ попадают.
		#
		# Циклическая ловушка, о которой стоит помнить: в backup.env лежат ключи
		# доступа к самому R2. Чтобы скачать копию, они нужны ДО неё, поэтому
		# ключи R2 и закрытый ключ age обязаны жить в менеджере паролей — бэкап
		# их не заменяет.
		cfg_tar="$tmp_dir/ether-config.tar"
		cfg_files=()
		for f in "$CONFIG" "$ENV_FILE" "${FCM_CREDENTIALS:-/etc/ether/fcm.json}"; do
			[[ -r "$f" ]] && cfg_files+=("$f")
		done
		if ((${#cfg_files[@]})); then
			tar -cf "$cfg_tar" -C / "${cfg_files[@]#/}"
			age -r "$AGE_RECIPIENT" -o "$cfg_tar.age" "$cfg_tar"
		fi

		dated="ether-$(date +%F).db.gz.age"
		put() { rclone --retries 3 -q copyto "$1" "${BACKUP_REMOTE%/}/$2"; }
		if put "$enc" "ether-latest.db.gz.age" && put "$enc" "$dated"; then
			echo "копия выгружена: ${BACKUP_REMOTE%/}/${dated}"
			if [[ -f "$cfg_tar.age" ]]; then
				if put "$cfg_tar.age" "ether-config-latest.tar.age" &&
					put "$cfg_tar.age" "ether-config-$(date +%F).tar.age"; then
					echo "конфиг выгружен"
				else
					alert "база выгружена, а конфиг — нет (смотри journalctl -u ether-backup)"
				fi
			fi
		else
			alert "не удалось выгрузить копию в ${BACKUP_REMOTE} (на сервере копия сохранена)"
		fi
	fi
fi

echo "бэкап готов: $BACKUP ($size); свободно на диске: ${free_pct}%"
