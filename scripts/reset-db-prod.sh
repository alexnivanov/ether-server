#!/usr/bin/env bash
# Сброс базы на ПРОД-сервере.
# Запускать руками на самой машине (root/sudo).
# Схема пересоздаётся при старте.

# Backup if needed
cp /opt/ether-server/ether.prod.db ~/ether.prod.db.bak

sudo systemctl stop ether-server
sudo rm -f /opt/ether-server/ether.prod.db /opt/ether-server/ether.prod.db-wal /opt/ether-server/ether.prod.db-shm
sudo systemctl start ether-server
