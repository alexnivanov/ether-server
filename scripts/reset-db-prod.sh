#!/usr/bin/env bash
# Сброс базы на ПРОД-сервере.
# Запускать руками на самой машине (root/sudo).
# Схема пересоздаётся при старте.

# Backup if needed
cp /var/lib/ether/ether.prod.db ~/ether.prod.db.bak

sudo systemctl stop ether-server
sudo rm -f /var/lib/ether/ether.prod.db /var/lib/ether/ether.prod.db-wal /var/lib/ether/ether.prod.db-shm
sudo systemctl start ether-server
