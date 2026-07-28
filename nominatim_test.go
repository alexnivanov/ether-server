//go:build live

package main

import "testing"

// Регрессионные проверки для NominatimGeocoder на локациях, которые уже
// сверялись руками — порт nominatim_hierarchy.test.js из ether-research
// (см. CLAUDE.md того репозитория "Сделано"). Бьют по публичному Nominatim —
// медленные реальные сетевые запросы, но маппинг rank_address/admin_level
// больше проверить негде. Один общий *NominatimGeocoder на все кейсы: его
// внутренний мьютекс сам держит паузу 1.1с между запросами (лимит публичного
// сервера), как ручной sleep между test() в JS-версии.
//
// За build-тегом live: usage policy публичного Nominatim запрещает
// автоматизированную нагрузку, а CI гоняет тесты на каждый push — незачем
// дёргать чужой сервер на каждый коммит. Запуск вручную: go test -tags live .
func TestChannelsKnownLocations(t *testing.T) {
	g := NewNominatimGeocoder()

	cases := []struct {
		name       string
		lat, lng   float64
		want       map[string]string // level → ID канала
		wantAbsent []string          // уровни, которых в наборе быть не должно
	}{
		{
			name: "Россия — Красная площадь: Район не путается с числовой адресной сеткой кварталов",
			lat:  55.7558, lng: 37.6173,
			want: map[string]string{
				"country":  "RU",
				"city":     "relation/2555133",
				"district": "relation/1257786", // Тверской район
				"quarter":  "relation/4045505", // Китай-город
			},
			// город фед. значения: субъект RU-MOW = сам город, слот погашен
			wantAbsent: []string{"region"},
		},
		{
			name: "Россия — Ходынское поле: второй ориентир без числовой сетки на пути",
			lat:  55.790535, lng: 37.525334,
			want: map[string]string{
				"country":  "RU",
				"city":     "relation/2555133",
				"district": "relation/445299", // Хорошёвский район
				"quarter":  "node/2331522062", // Ходынское поле
			},
			wantAbsent: []string{"region"},
		},
		{
			name: "Россия — Санкт-Петербург: второй город фед. значения, Область так же погашена",
			lat:  59.9343, lng: 30.3351,
			want: map[string]string{
				"country":  "RU",
				"city":     "relation/421007",
				"district": "relation/1198055",
			},
			wantAbsent: []string{"region"}, // RU-SPE = сам город
		},
		{
			// Контрольная пара к Москве: гасить Область можно только когда субъект
			// и есть сам город. Химки лежат в Московской области — субъект здесь
			// реальный родитель, а не дубль города.
			name: "Россия — Химки: город ВНУТРИ области, RU-MOS обязана остаться",
			lat:  55.897, lng: 37.4297,
			want: map[string]string{
				"country":  "RU",
				"region":   "RU-MOS",
				"city":     "relation/181326",
				"district": "node/4864421769",
			},
		},
		{
			name: "ФРГ — Франкфурт: Stadtteil/Ortsbezirk (administrative), а не neighbourhood",
			lat:  50.1109, lng: 8.6821,
			want: map[string]string{
				"country":  "DE",
				"region":   "DE-HE",
				"city":     "relation/62400",
				"district": "relation/11042433", // Innenstadt 1
				"quarter":  "relation/4209934",  // Altstadt
			},
		},
		{
			// Второй сторож правила «фед. город»: тут штат и город тоже совпадают
			// по имени (New York / New York), но город — boundary/administrative
			// внутри штата, а не ярлык самого субъекта. Гасить US-NY нельзя.
			name: "США — Манхэттен: County-тир отсекается, одноимённый штат НЕ гасится",
			lat:  40.7484, lng: -73.9857,
			want: map[string]string{
				"country":  "US",
				"region":   "US-NY",
				"city":     "relation/175905",
				"district": "relation/7340060", // Manhattan Community Board 5
				"quarter":  "node/2511053877",  // Koreatown
			},
		},
		{
			// Известная асимметрия, а не желаемый результат. Берлин — тоже город
			// фед. значения, но субъект и город в OSM — ОДНА relation, поэтому
			// дубль снимает раннее исключение («Город без единицы, ушедшей в
			// Область»): пустеет Город, а ISO-код остаётся. У Москвы объекта два,
			// и по правилу фед. города пустеет Область. Итог — один канал в обоих
			// случаях, но лежит он в разных слотах. Сводить в пользу Города нельзя:
			// Токио потеряет JP-13 (там субъект — настоящая префектура).
			// Подгородские слоты здесь НЕ проверяем: на этой точке публичный
			// Nominatim отдаёт разные цепочки в зависимости от Accept-Encoding
			// (gzip-вариант ответа в кеше протухший и не содержит мельчайшую
			// единицу Nikolaiviertel, несжатый содержит). Go запрашивает gzip
			// автоматически, JS-прототип — нет, отсюда расхождение портов при
			// одинаковой логике. Пинить такие ID бессмысленно, пока источник —
			// публичный сервер; сам факт — довод за свой Nominatim.
			name: "ФРГ — Берлин: город-государство одним объектом OSM, расклад зеркальный Москве",
			lat:  52.52, lng: 13.405,
			want: map[string]string{
				"country": "DE",
				"region":  "DE-BE",
			},
			wantAbsent: []string{"city"},
		},
		{
			name: "Сингапур — Область пуста (нет ISO 3166-2 субъекта), лишние тиры не протекают в неё",
			lat:  1.2868, lng: 103.8545,
			want: map[string]string{
				"country":  "SG",
				"city":     "relation/17140517",
				"district": "node/8777036099",   // Civic District
				"quarter":  "relation/19910266", // Clifford Pier
			},
			wantAbsent: []string{"region"},
		},
		{
			name: "Монако — ISO 3166-2 субъект лежит на admin_level 10, а не 4",
			lat:  43.7384, lng: 7.4246,
			want: map[string]string{
				"country":  "MC",
				"region":   "MC-MC", // раньше уходило в osm_id — region_iso читался только из lvl4
				"city":     "node/1790048269",
				"district": "relation/2220322", // Monaco (administrative)
				"quarter":  "relation/5986438", // Monte-Carlo
			},
		},
		{
			// Известное ограничение, не правильный результат: 港区 (Minato,
			// admin_level 7) не попадает в диапазон rank 10–16 Города и не несёт
			// ISO-кода, поэтому Город пуст, а sub-city съезжает на chōme. Нужны
			// per-country правила sub-city (admin_level → слот) для Восточной
			// Азии — см. TODO в ether-meta. Тест фиксирует ТЕКУЩЕЕ поведение,
			// чтобы будущий фикс был осознанным диффом.
			name: "Токио (Минато) — известное ограничение: ward выпадает из Города, sub-city съезжает на chōme",
			lat:  35.6586, lng: 139.7454,
			want: map[string]string{
				"country":  "JP",
				"region":   "JP-13",             // 東京都
				"district": "relation/18180831", // 芝公園 (chōme-уровень, не 港区)
				"quarter":  "relation/18180827", // 芝公園四丁目
			},
			wantAbsent: []string{"city"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chs, err := g.Channels(tc.lat, tc.lng)
			if err != nil {
				t.Fatalf("Channels: %v", err)
			}

			byLevel := make(map[string]Channel, len(chs))
			for _, c := range chs {
				byLevel[c.Level] = c
			}

			// планета есть всегда и первой — она не зависит от координат
			if len(chs) == 0 || chs[0] != PlanetChannel {
				t.Errorf("первый канал = %+v, want %+v", chs, PlanetChannel)
			}

			for level, id := range tc.want {
				got, ok := byLevel[level]
				if !ok {
					t.Errorf("%s: отсутствует, хотели ID %q", level, id)
					continue
				}
				if got.ID != id {
					t.Errorf("%s: got %q (%s), want %q", level, got.ID, got.Name, id)
				}
			}
			for _, level := range tc.wantAbsent {
				if got, ok := byLevel[level]; ok {
					t.Errorf("%s: got %q (%s), want отсутствие слота", level, got.ID, got.Name)
				}
			}
		})
	}
}
