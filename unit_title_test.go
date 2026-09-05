package main

import (
	"strings"
	"testing"
	"time"
)

// Подпись обязана называть саму единицу, а не слот, в который она попала.
// Кейсы взяты из живых ответов Nominatim: Карцево и Истра — та точка, на
// которой правило и понадобилось (56.045031, 36.708778); Москва и Тверской —
// сторожа обратного случая, где типового слова нет и подпись слота верна.
func TestUnitTitle(t *testing.T) {
	cases := []struct {
		name      string
		entry     nomAddressEntry
		slotLabel string
		wantLabel string
		wantName  string
		wantGap   bool // единица попадёт в рабочий список пробелов словаря
	}{
		{
			name:      "деревня вместо Района",
			entry:     nomAddressEntry{LocalName: "Карцево", Class: "place", Type: "hamlet"},
			slotLabel: "Район", wantLabel: "Деревня", wantName: "Карцево",
		},
		{
			name:      "муниципальный округ вместо Города, слово уходит из имени",
			entry:     nomAddressEntry{LocalName: "муниципальный округ Истра", Class: "boundary", Type: "administrative", AdminLevel: 6},
			slotLabel: "Город", wantLabel: "Муниципальный округ", wantName: "Истра",
		},
		{
			name:      "городской округ",
			entry:     nomAddressEntry{LocalName: "городской округ Химки", Class: "boundary", Type: "administrative"},
			slotLabel: "Город", wantLabel: "Городской округ", wantName: "Химки",
		},
		{
			name:      "республика вместо Области",
			entry:     nomAddressEntry{LocalName: "Республика Татарстан", Class: "boundary", Type: "administrative"},
			slotLabel: "Область", wantLabel: "Республика", wantName: "Татарстан",
		},
		{
			name:      "город остаётся Городом",
			entry:     nomAddressEntry{LocalName: "Москва", Class: "boundary", Type: "administrative"},
			slotLabel: "Город", wantLabel: "Город", wantName: "Москва",
		},
		{
			name:      "район остаётся Районом",
			entry:     nomAddressEntry{LocalName: "Тверской", Class: "boundary", Type: "administrative"},
			slotLabel: "Район", wantLabel: "Район", wantName: "Тверской",
		},
		{
			name:      "квартал по тегу place",
			entry:     nomAddressEntry{LocalName: "Китай-город", Class: "place", Type: "neighbourhood"},
			slotLabel: "Квартал", wantLabel: "Квартал", wantName: "Китай-город",
		},
		{
			name:      "длинный тип примеряется раньше короткого",
			entry:     nomAddressEntry{LocalName: "посёлок городского типа Нахабино", Class: "boundary", Type: "administrative"},
			slotLabel: "Город", wantLabel: "Посёлок городского типа", wantName: "Нахабино",
		},
		{
			name:      "типовое слово не в начале — имя целиком",
			entry:     nomAddressEntry{LocalName: "Красное Село", Class: "place", Type: "town"},
			slotLabel: "Город", wantLabel: "Город", wantName: "Красное Село",
		},
		{
			name:      "незнакомая страна и тип — подпись слота",
			entry:     nomAddressEntry{LocalName: "Bahnhofsviertel", Class: "boundary", Type: "administrative"},
			slotLabel: "Квартал", wantLabel: "Квартал", wantName: "Bahnhofsviertel",
		},
		{
			// Дыра в словаре типов: подпись слота могла оказаться неправдой,
			// и слово дописывается в placeLabels одной строкой.
			name:      "незнакомый тип place — в список пробелов",
			entry:     nomAddressEntry{LocalName: "Пустошь", Class: "place", Type: "locality"},
			slotLabel: "Район", wantLabel: "Район", wantName: "Пустошь", wantGap: true,
		},
		{
			// Дыра в правиле, а не в словаре: тип стоит в конце имени, а ищем
			// мы его в начале. Превратить в подпись нельзя, не решив, что
			// делать с прилагательным, — поэтому в список, а не в label.
			name:      "типовое слово в конце имени — в список пробелов",
			entry:     nomAddressEntry{LocalName: "Клинский район", Class: "boundary", Type: "administrative", AdminLevel: 6},
			slotLabel: "Город", wantLabel: "Город", wantName: "Клинский район", wantGap: true,
		},
		{
			// Суффикс совпал с подписью слота — чинить нечего, и в список это
			// не идёт: иначе «Московская область» заняла бы его целиком.
			name:      "суффикс совпадает с подписью слота — не пробел",
			entry:     nomAddressEntry{LocalName: "Московская область", Class: "boundary", Type: "administrative", AdminLevel: 4},
			slotLabel: "Область", wantLabel: "Область", wantName: "Московская область",
		},
		{
			// А тут слово другое: «Край» точнее «Области», это настоящий пробел.
			name:      "край в слоте Области — пробел",
			entry:     nomAddressEntry{LocalName: "Краснодарский край", Class: "boundary", Type: "administrative", AdminLevel: 4},
			slotLabel: "Область", wantLabel: "Область", wantName: "Краснодарский край", wantGap: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, name, gap := unitTitle(&c.entry, c.slotLabel)
			if label != c.wantLabel || name != c.wantName {
				t.Errorf("unitTitle = %q / %q, want %q / %q", label, name, c.wantLabel, c.wantName)
			}
			if (gap != nil) != c.wantGap {
				t.Errorf("пробел словаря = %v, want %v", gap != nil, c.wantGap)
			}
		})
	}
}

// Единственное слово, которое не должно оставить имя пустым: если после типа
// ничего нет, имя важнее подписи — иначе канал остался бы безымянным.
func TestUnitTitleKeepsNameWhenNothingLeft(t *testing.T) {
	e := nomAddressEntry{LocalName: "Республика", Class: "boundary", Type: "administrative"}
	label, name, _ := unitTitle(&e, "Область")
	if name != "Республика" || label != "Область" {
		t.Errorf("unitTitle = %q / %q, want Область / Республика", label, name)
	}
}

// Таблица пробелов — рабочий список, а не журнал: повтор той же единицы двигает
// счётчик, а не плодит строки, и после правки словаря список сам пустеет.
func TestUnmappedUnitStore(t *testing.T) {
	store := openTestStore(t)
	gap := unitGap{Class: "place", Type: "locality", Name: "Пустошь"}

	for i := 0; i < 3; i++ {
		if err := store.SaveUnmappedUnit(gap, "district", "RU"); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	now := time.Now().UnixMilli()
	rows, err := store.WeeklyStats(now-int64(time.Hour/time.Millisecond), now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(rows.UnmappedUnits) != 1 || rows.UnmappedUnits[0].Count != 3 {
		t.Fatalf("список = %+v, want одна строка со счётчиком 3", rows.UnmappedUnits)
	}
	if got := rows.UnmappedUnits[0].Key; !strings.Contains(got, "locality") || !strings.Contains(got, "Пустошь") {
		t.Errorf("ключ строки = %q, want тип и имя вместе", got)
	}

	// Правка словаря меняет отпечаток: строки прежнего словаря уходят на старте.
	n, err := store.DeleteUnmappedUnitsExcept("другой-словарь")
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Errorf("вынесено строк %d, want 1", n)
	}
	// А свой отпечаток список переживает — иначе он обнулялся бы каждым стартом.
	if err := store.SaveUnmappedUnit(gap, "district", "RU"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n, err := store.DeleteUnmappedUnitsExcept(unitDictHash()); err != nil || n != 0 {
		t.Errorf("вынесено %d (err %v), want 0 — словарь не менялся", n, err)
	}
}
