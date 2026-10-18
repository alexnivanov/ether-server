#!/usr/bin/env bash
# Деплой ether-server на прод: собрать статический бинарник под Linux, залить
# на сервер, атомарно подменить и перезапустить systemd-сервис.
#
# Скрипт трогает только бинарник (/opt/ether/ether-server). Конфиг
# (/etc/ether/config.prod.yaml, секреты) и база (/var/lib/ether/ether.prod.db,
# состояние) живут на сервере и не трогаются. Деплой = новый бинарник + рестарт.
#
# Настройка через окружение (или инлайном: SSH_HOST=... ./deploy.sh):
#   SSH_HOST     обязателен, напр. ether (алиас из ~/.ssh/config) или root@host
#   REMOTE_DIR   каталог сервиса на сервере (по умолчанию /opt/ether)
#   OWNER        владелец бинарника на сервере (по умолчанию ether)
#   SERVICE      имя systemd-юнита (по умолчанию ether-server)
#   ARCH         архитектура сервера: amd64 | arm64 (по умолчанию amd64)
#   DOMAIN       домен для health-check после рестарта (по умолчанию etherapp.ru)
#   API_PREFIX   префикс API на домене (по умолчанию /api — Caddy проксирует в
#                сервер только /api/*, срезая префикс; остальное — статика
#                ether-web). Пусто = сервер отдаёт домен целиком.
#   SKIP_CHECKS  =1 пропустить gofmt/vet/test перед сборкой (не рекомендуется)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

SSH_HOST="${SSH_HOST:?задай SSH_HOST, напр. SSH_HOST=ether}"
REMOTE_DIR="${REMOTE_DIR:-/opt/ether}"
OWNER="${OWNER:-ether}"
SERVICE="${SERVICE:-ether-server}"
ARCH="${ARCH:-amd64}"
DOMAIN="${DOMAIN:-etherapp.ru}"
API_PREFIX="${API_PREFIX-/api}"

# ── проверки перед сборкой (то же, что CI: format/lint/test) ──
if [[ "${SKIP_CHECKS:-}" != "1" ]]; then
	echo "==> gofmt"
	fmt_out="$(gofmt -l .)"
	if [[ -n "$fmt_out" ]]; then
		echo "не отформатировано (gofmt -w .):" >&2
		echo "$fmt_out" >&2
		exit 1
	fi
	echo "==> go vet"
	go vet ./...
	echo "==> go test"
	go test -count=1 ./...
fi

# ── сборка статического бинарника под Linux ──
# CGO_ENABLED=0: modernc/sqlite — чистый Go, cgo не нужен; бинарник не зависит
# от glibc сервера. -trimpath/-ldflags — как в разделе про прод-сборку.
version="$(git rev-parse --short HEAD)$(git diff --quiet || echo -dirty)"
out="$(mktemp -d)/ether-server"
echo "==> build linux/${ARCH} (${version})"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
	go build -trimpath -ldflags="-s -w -X main.version=${version}" -o "$out" .

# ── доставка и атомарная подмена ──
# scp льём в /tmp (мир-доступный на запись), а не сразу в /opt — тот принадлежит
# root, и scp туда падает с "No such file or directory". Затем `sudo install`
# ставит бинарник на место атомарно и сразу с нужным владельцем/правами
# (install заменяет файл через временный + rename, старый не «полуперезаписан»).
remote_bin="${REMOTE_DIR}/ether-server"
echo "==> upload → ${SSH_HOST}:/tmp/ether-server.new"
# rsync, а не scp: на плохом канале (потери, сотни мс задержки) scp полз и
# зависал так, что не выходил по Ctrl+C. --partial оставляет на сервере
# недокачанное, и следующая попытка продолжает с места обрыва. Зависание ловит
# ssh-keepalive: сервер молчит ~минуту — соединение рвётся, цикл пробует снова.
# Общего таймаута на загрузку нет: медленная, но идущая загрузка не прерывается.
upload_tries=5
for try in $(seq 1 "$upload_tries"); do
	if rsync --partial --progress \
		-e "ssh -o ServerAliveInterval=15 -o ServerAliveCountMax=4" \
		"$out" "${SSH_HOST}:/tmp/ether-server.new"; then
		break
	fi
	if [ "$try" -eq "$upload_tries" ]; then
		echo "загрузка не удалась за ${upload_tries} попыток" >&2
		exit 1
	fi
	echo "==> обрыв, попытка $((try + 1))/${upload_tries}…"
	sleep 5
done

# Скрипты бэкапа и алерта о падениях едут вместе с бинарником — иначе версия на
# сервере тихо разъедется с репозиторием. Юниты ether-backup.service и ether-server.service
# (ExecStopPost) запускают именно эти файлы.
echo "==> upload backup.sh, notify-failure.sh"
scp -q scripts/backup.sh "${SSH_HOST}:/tmp/ether-backup.sh"
scp -q scripts/notify-failure.sh "${SSH_HOST}:/tmp/ether-notify-failure.sh"

echo "==> install + restart ${SERVICE}"
ssh "$SSH_HOST" "set -e
	sudo install -o ${OWNER} -g ${OWNER} -m 0755 /tmp/ether-server.new ${remote_bin}
	rm -f /tmp/ether-server.new
	sudo install -o root -g root -m 0755 /tmp/ether-backup.sh ${REMOTE_DIR}/backup.sh
	rm -f /tmp/ether-backup.sh
	sudo install -o root -g root -m 0755 /tmp/ether-notify-failure.sh ${REMOTE_DIR}/notify-failure.sh
	rm -f /tmp/ether-notify-failure.sh
	sudo systemctl restart ${SERVICE}
	sleep 1
	sudo systemctl is-active --quiet ${SERVICE} || { sudo journalctl -u ${SERVICE} -n 30 --no-pager; exit 1; }"

# ── health-check через публичный домен (проверяет бинарник + Caddy + TLS) ──
# /health пингует базу и отдаёт версию — то есть проверяет не только «процесс
# отвечает», но и «база читается». Идём через API_PREFIX: на домене живёт ещё и
# статика ether-web, поэтому без префикса ответит она (404), а не сервер.
health_url="https://${DOMAIN}${API_PREFIX}/health"
echo "==> health-check ${health_url}"
if body=$(curl -fsS --max-time 10 "$health_url"); then
	echo "✓ задеплоено: ${version}"
	echo "  health: ${body}"
else
	echo "✗ сервис перезапущен, но health-check не прошёл — проверь Caddy/логи" >&2
	exit 1
fi
