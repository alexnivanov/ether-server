package main

import (
	"log/slog"
	"time"
)

const (
	// messageTTL — сколько живёт сообщение. Гео-чат живой: неделя истории даёт
	// вернувшемуся в район прочитать, что тут было, дальше — мусор.
	messageTTL = 7 * 24 * time.Hour
	// geocodeRequestTTL — сколько держим строки об обращениях к Nominatim. Месяц
	// даёт сравнить недели в сводке и посмотреть глазами, когда была очередь;
	// дольше хранить нечего — это диагностика нагрузки, а не история.
	geocodeRequestTTL = 30 * 24 * time.Hour
	// как часто подчищаем. Точность в пределах часа для недельного TTL не важна,
	// зато редкие проходы дешевле для SQLite (один писатель).
	cleanupInterval = time.Hour
)

// startCleanup раз в cleanupInterval удаляет просроченное: сообщения старше
// messageTTL и строки статистики геокодинга старше geocodeRequestTTL. Первый
// проход — сразу при старте: иначе после рестарта просроченное висело бы ещё час.
// Ошибка не фатальна — следующий проход попробует снова, и одна неудача не должна
// отменять вторую уборку. Запускать в отдельной горутине (см. main).
func startCleanup(store *Store) {
	for {
		if n, err := store.DeleteMessagesOlderThan(messageTTL); err != nil {
			slog.Error("delete old messages", "err", err)
		} else if n > 0 {
			slog.Info("old messages deleted", "count", n, "older_than", messageTTL)
		}
		if n, err := store.DeleteGeocodeRequestsOlderThan(geocodeRequestTTL); err != nil {
			slog.Error("delete old geocode requests", "err", err)
		} else if n > 0 {
			slog.Info("old geocode requests deleted", "count", n, "older_than", geocodeRequestTTL)
		}
		time.Sleep(cleanupInterval)
	}
}
