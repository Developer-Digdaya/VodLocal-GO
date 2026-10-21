// Package scheduler menjadwalkan sinkronisasi otomatis berdasarkan ekspresi
// cron lima field: menit, jam, hari-bulan, bulan dan hari-minggu.
//
// Tata letak berkas:
//   - cron.go      : parsing dan pencocokan ekspresi cron,
//   - scheduler.go : loop penjadwal yang membaca tabel app_settings.
//
// Penjadwal tidak menyentuh jaringan maupun filesystem media; ia hanya membaca
// dua setting dari database lalu memanggil SyncRunner (biasanya *syncjob.Syncer)
// di goroutine terpisah supaya loop tetap hidup selama sinkronisasi berjalan.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// fieldSpec menerangkan satu field cron: nama untuk pesan error, rentang nilai
// yang sah, serta alias nilai (dipakai hari-minggu: 7 sama dengan 0).
type fieldSpec struct {
	name  string
	lo    int
	hi    int
	alias map[int]int
}

// Spesifikasi kelima field. Isinya mengikuti urutan penulisan ekspresi cron.
var (
	minuteSpec = fieldSpec{name: "menit", lo: 0, hi: 59}
	hourSpec   = fieldSpec{name: "jam", lo: 0, hi: 23}
	daySpec    = fieldSpec{name: "hari-bulan", lo: 1, hi: 31}
	monthSpec  = fieldSpec{name: "bulan", lo: 1, hi: 12}
	weekSpec   = fieldSpec{name: "hari-minggu", lo: 0, hi: 7, alias: map[int]int{7: 0}}

	// fieldOrder adalah urutan field pada ekspresi: menit jam hari-bulan
	// bulan hari-minggu.
	fieldOrder = [...]fieldSpec{minuteSpec, hourSpec, daySpec, monthSpec, weekSpec}
)

// fieldNames adalah daftar nama field untuk pesan error jumlah field.
const fieldNames = "menit jam hari-bulan bulan hari-minggu"

// Schedule adalah satu ekspresi cron lima field yang sudah divalidasi.
//
// Nilai nol Schedule (Schedule{}) tidak cocok dengan waktu mana pun, sehingga
// pemanggil yang lupa mengecek error Parse tidak akan menjadwalkan sesuatu
// secara tak sengaja.
type Schedule struct {
	minute uint64
	hour   uint64
	day    uint64
	month  uint64
	week   uint64

	// expr adalah ekspresi asli dengan spasi dinormalisasi.
	expr string
	// loc adalah zona waktu yang dipakai Matches; nil berarti time.Local.
	loc *time.Location
}

// Parse mengurai ekspresi cron lima field. Format yang didukung: "*", angka,
// rentang "a-b", langkah "*/n" dan "a-b/n", serta daftar yang dipisah koma
// (mis. "0 0 1,15 * *").
//
// Ekspresi yang tidak sah dikembalikan sebagai error yang menyebut bagian mana
// yang bermasalah, mis. `cron: ekspresi "60 * * * *": menit: nilai 60 di luar
// rentang 0-59`.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != len(fieldOrder) {
		return Schedule{}, fmt.Errorf("cron: ekspresi %q harus terdiri dari %d field (%s), dapat %d",
			strings.TrimSpace(expr), len(fieldOrder), fieldNames, len(fields))
	}

	var bits [len(fieldOrder)]uint64
	for i, spec := range fieldOrder {
		mask, err := parseField(fields[i], spec)
		if err != nil {
			return Schedule{}, fmt.Errorf("cron: ekspresi %q: %w", strings.Join(fields, " "), err)
		}
		bits[i] = mask
	}

	return Schedule{
		minute: bits[0],
		hour:   bits[1],
		day:    bits[2],
		month:  bits[3],
		week:   bits[4],
		expr:   strings.Join(fields, " "),
		loc:    time.Local,
	}, nil
}

// String mengembalikan ekspresi asli dengan spasi dinormalisasi menjadi satu
// spasi tunggal. Schedule nol mengembalikan string kosong.
func (s Schedule) String() string { return s.expr }

// Matches melaporkan apakah jadwal cocok dengan t pada resolusi menit. Waktu
// dikonversi lebih dahulu ke zona Schedule (default time.Local); detik dan
// pecahan detik diabaikan.
func (s Schedule) Matches(t time.Time) bool {
	if s.empty() {
		return false
	}

	lt := t.In(s.location())
	return hasBit(s.minute, lt.Minute()) &&
		hasBit(s.hour, lt.Hour()) &&
		hasBit(s.day, lt.Day()) &&
		hasBit(s.month, int(lt.Month())) &&
		hasBit(s.week, int(lt.Weekday()))
}

// location mengembalikan zona waktu Schedule, default time.Local.
func (s Schedule) location() *time.Location {
	if s.loc == nil {
		return time.Local
	}
	return s.loc
}

// withLocation mengembalikan salinan Schedule yang mencocokkan waktu pada loc.
// loc nil berarti time.Local. Penjadwal memakai ini supaya jadwal dievaluasi
// pada Config.Location, bukan pada time.Local proses.
func (s Schedule) withLocation(loc *time.Location) Schedule {
	if loc == nil {
		loc = time.Local
	}
	s.loc = loc
	return s
}

// empty melaporkan apakah Schedule belum diisi (nilai nol).
func (s Schedule) empty() bool {
	return s.minute == 0 && s.hour == 0 && s.day == 0 && s.month == 0 && s.week == 0
}

// parseField mengurai satu field yang boleh berisi daftar dipisah koma.
func parseField(field string, spec fieldSpec) (uint64, error) {
	if strings.TrimSpace(field) == "" {
		return 0, fmt.Errorf("%s: field kosong", spec.name)
	}

	var mask uint64
	for _, part := range strings.Split(field, ",") {
		bits, err := parseElement(strings.TrimSpace(part), spec)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", spec.name, err)
		}
		mask |= bits
	}
	return mask, nil
}

// parseElement mengurai satu elemen field: "*", angka, rentang, atau salah
// satunya dengan langkah "/n". Error yang dikembalikan belum menyebut nama
// field; parseField yang menambahkannya.
func parseElement(el string, spec fieldSpec) (uint64, error) {
	if el == "" {
		return 0, fmt.Errorf("elemen daftar kosong")
	}

	base := el
	step := 1
	hasStep := false
	if i := strings.IndexByte(el, '/'); i >= 0 {
		base = strings.TrimSpace(el[:i])
		rawStep := strings.TrimSpace(el[i+1:])
		n, err := strconv.Atoi(rawStep)
		if err != nil {
			return 0, fmt.Errorf("langkah %q bukan angka", rawStep)
		}
		if n <= 0 {
			return 0, fmt.Errorf("langkah %q harus lebih besar dari nol", rawStep)
		}
		step = n
		hasStep = true
	}

	var start, end int
	switch {
	case base == "*":
		start, end = spec.lo, spec.hi
	case strings.Contains(base, "-"):
		a, b, err := parseRange(base, spec)
		if err != nil {
			return 0, err
		}
		start, end = a, b
	default:
		v, err := atoiInRange(base, spec)
		if err != nil {
			return 0, err
		}
		start, end = v, v
		// "a/n" berarti a sampai batas atas dengan langkah n.
		if hasStep {
			end = spec.hi
		}
	}

	var mask uint64
	for v := start; v <= end; v += step {
		mask |= bitAt(spec, v)
	}
	return mask, nil
}

// parseRange mengurai rentang "a-b".
func parseRange(raw string, spec fieldSpec) (int, int, error) {
	loStr, hiStr, _ := strings.Cut(raw, "-")
	if strings.Contains(hiStr, "-") {
		return 0, 0, fmt.Errorf("rentang %q tidak valid", raw)
	}

	a, err := atoiInRange(strings.TrimSpace(loStr), spec)
	if err != nil {
		return 0, 0, fmt.Errorf("rentang %q: %w", raw, err)
	}
	b, err := atoiInRange(strings.TrimSpace(hiStr), spec)
	if err != nil {
		return 0, 0, fmt.Errorf("rentang %q: %w", raw, err)
	}
	if a > b {
		return 0, 0, fmt.Errorf("rentang %q terbalik, batas bawah melebihi batas atas", raw)
	}
	return a, b, nil
}

// atoiInRange mengubah raw menjadi angka dan memastikan nilainya di rentang
// field. Pesan error tidak menyebut nama field.
func atoiInRange(raw string, spec fieldSpec) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("nilai kosong")
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q bukan angka", raw)
	}
	if v < spec.lo || v > spec.hi {
		return 0, fmt.Errorf("nilai %d di luar rentang %d-%d", v, spec.lo, spec.hi)
	}
	return v, nil
}

// bitAt mengembalikan bit untuk nilai v, memakai nilai kanonik bila field punya
// alias (hari-minggu 7 → 0).
func bitAt(spec fieldSpec, v int) uint64 {
	if canon, ok := spec.alias[v]; ok {
		v = canon
	}
	return 1 << uint(v)
}

// hasBit melaporkan apakah bit v terpasang pada mask.
func hasBit(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }
