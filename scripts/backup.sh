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
#
# ВАЖНО про SEND_TO_TELEGRAM: копия содержит персональные данные (Telegram id,
# имена, сообщения), то есть выгрузка — это передача их в Telegram. Канал
# приватный, но решение осознанное: по умолчанию выключено.

set -euo pipefail

DB="${DB:-/var/lib/ether/ether.prod.db}"
BACKUP="${BACKUP:-/var/lib/ether/backups/ether-latest.db.gz}"
CONFIG="${CONFIG:-/etc/ether/config.prod.json}"
MIN_FREE_PCT="${MIN_FREE_PCT:-15}"

# ── алерты в служебный Telegram-канал (тот же, что у модерации) ──
# Токен и chat_id читаем из конфига сервера, чтобы не дублировать секреты.
tg_token=""
tg_chat=""
if [[ -r "$CONFIG" ]]; then
	tg_token=$(python3 -c "import json;print(json.load(open('$CONFIG')).get('telegram_notify_token',''))" 2>/dev/null || true)
	tg_chat=$(python3 -c "import json;print(json.load(open('$CONFIG')).get('telegram_notify_chat_id',''))" 2>/dev/null || true)
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

echo "бэкап готов: $BACKUP ($size); свободно на диске: ${free_pct}%"
