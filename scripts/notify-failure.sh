#!/usr/bin/env bash
# ExecStopPost юнита ether-server: алерт в служебный Telegram-канал, когда сервер
# остановился аварийно (падение, OOM-kill). systemd сам поднимает его заново
# (Restart=on-failure), так что без алерта падение прошло бы молча — увидеть его
# можно было бы только случайно в журнале. Ставится как /opt/ether/notify-failure.sh,
# заливается вместе с бинарником (scripts/deploy.sh).
#
# Причину systemd передаёт в окружении: SERVICE_RESULT (success, exit-code, signal,
# oom-kill, …), EXIT_CODE (exited, killed, dumped) и EXIT_STATUS (код или сигнал).
# Штатный stop/restart при деплое — это success: Go-сервер умирает от SIGTERM, а его
# systemd считает чистым завершением.
#
# Проверять имитацией только так: sudo kill -9 $(systemctl show -p MainPID --value ether-server).
# `systemctl kill` бьёт по всем процессам юнита и убивает заодно и этот скрипт.

set -u

[[ "${SERVICE_RESULT:-success}" == "success" ]] && exit 0

CONFIG="${CONFIG:-/etc/ether/config.prod.yaml}"

# Те же две плоские строки YAML, что читает backup.sh
yaml_value() {
	sed -n "s/^$1:[[:space:]]*//p" "$CONFIG" 2>/dev/null |
		head -1 |
		sed -e 's/[[:space:]]*$//' -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'$/\1/"
}
token=$(yaml_value telegram_notify_token)
chat=$(yaml_value telegram_notify_chat_id)

text="⚠️ ether-server на $(hostname) остановился аварийно: ${SERVICE_RESULT} (${EXIT_CODE:-?} ${EXIT_STATUS:-?}). systemd перезапускает сервис. Причина: journalctl -u ether-server, OOM: journalctl -k | grep -i oom"
echo "$text" >&2

if [[ -n "$token" && -n "$chat" ]]; then
	curl -sS -m 15 -o /dev/null \
		-X POST "https://api.telegram.org/bot${token}/sendMessage" \
		--data-urlencode "chat_id=${chat}" \
		--data-urlencode "text=${text}" || true
fi

# Никогда не падаем: упавший ExecStopPost не должен мешать перезапуску
exit 0
