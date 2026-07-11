#!/usr/bin/env bash
# Сброс SQLite-базы окружения: удаляет ether.<env>.db (+ -wal/-shm).
# Использование: scripts/reset-db.sh [env]   (по умолчанию dev)
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

env="${1:-dev}"
db="ether.${env}.db"

read -r -p "удалить ${db} (+ -wal/-shm)? [y/N] " reply
[[ "$reply" =~ ^[Yy]$ ]] || { echo "отменено"; exit 1; }

rm -fv "$db" "${db}-wal" "${db}-shm"
