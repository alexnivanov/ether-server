#!/usr/bin/env bash
# Деплой ether-server на прод: собрать статический бинарник под Linux, залить
# на сервер, атомарно подменить и перезапустить systemd-сервис.
#
# Скрипт трогает только бинарник (/opt/ether/ether-server). Конфиг
# (/etc/ether/config.prod.json, секреты) и база (/var/lib/ether/ether.prod.db,
# состояние) живут на сервере и не трогаются. Деплой = новый бинарник + рестарт.
#
# Настройка через окружение (или инлайном: SSH_HOST=... scripts/deploy.sh):
#   SSH_HOST     обязателен, напр. ether (алиас из ~/.ssh/config) или root@host
#   REMOTE_DIR   каталог сервиса на сервере (по умолчанию /opt/ether)
#   OWNER        владелец бинарника на сервере (по умолчанию ether)
#   SERVICE      имя systemd-юнита (по умолчанию ether-server)
#   ARCH         архитектура сервера: amd64 | arm64 (по умолчанию amd64)
#   DOMAIN       домен для health-check после рестарта (по умолчанию etherapp.ru)
#   SKIP_CHECKS  =1 пропустить gofmt/vet/test перед сборкой (не рекомендуется)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

SSH_HOST="${SSH_HOST:?задай SSH_HOST, напр. SSH_HOST=ether}"
REMOTE_DIR="${REMOTE_DIR:-/opt/ether}"
OWNER="${OWNER:-ether}"
SERVICE="${SERVICE:-ether-server}"
ARCH="${ARCH:-amd64}"
DOMAIN="${DOMAIN:-etherapp.ru}"

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
scp -q "$out" "${SSH_HOST}:/tmp/ether-server.new"

echo "==> install + restart ${SERVICE}"
ssh "$SSH_HOST" "set -e
	sudo install -o ${OWNER} -g ${OWNER} -m 0755 /tmp/ether-server.new ${remote_bin}
	rm -f /tmp/ether-server.new
	sudo systemctl restart ${SERVICE}
	sleep 1
	sudo systemctl is-active --quiet ${SERVICE} || { sudo journalctl -u ${SERVICE} -n 30 --no-pager; exit 1; }"

# ── health-check через публичный домен (проверяет бинарник + Caddy + TLS) ──
# /history без авторизации отдаёт 200 даже на несуществующем канале.
echo "==> health-check https://${DOMAIN}/history"
if curl -fsS --max-time 10 "https://${DOMAIN}/history?channel=deploy-health-check" >/dev/null; then
	echo "✓ задеплоено: ${version}"
else
	echo "✗ сервис перезапущен, но health-check не прошёл — проверь Caddy/логи" >&2
	exit 1
fi
