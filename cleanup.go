package main

import (
	"log/slog"
	"time"
)

const (
	// messageTTL — сколько живёт сообщение. Гео-чат живой: неделя истории даёт
	// вернувшемуся в район прочитать, что тут было, дальше — мусор.
	messageTTL = 7 * 24 * time.Hour
	// как часто подчищаем. Точность в пределах часа для недельного TTL не важна,
	// зато редкие проходы дешевле для SQLite (один писатель).
	cleanupInterval = time.Hour
)

// startMessageCleanup раз в cleanupInterval удаляет сообщения старше messageTTL.
// Первый проход — сразу при старте: иначе после рестарта просроченные сообщения
// висели бы ещё час. Ошибка не фатальна — следующий проход попробует снова.
// Запускать в отдельной горутине (см. main).
func startMessageCleanup(store *Store) {
	for {
		if n, err := store.DeleteMessagesOlderThan(messageTTL); err != nil {
			slog.Error("delete old messages", "err", err)
		} else if n > 0 {
			slog.Info("old messages deleted", "count", n, "older_than", messageTTL)
		}
		time.Sleep(cleanupInterval)
	}
}
