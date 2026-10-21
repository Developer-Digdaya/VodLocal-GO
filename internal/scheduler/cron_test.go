package scheduler

import (
	"strings"
	"testing"
	"time"
)

// mustParse mem-parse expr dan menghentikan test bila gagal.
func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()

	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", expr, err)
	}
	return s
}

// utc mem-parse expr lalu mengikatnya ke UTC supaya pencocokan tidak
// bergantung pada zona waktu mesin uji.
func utc(t *testing.T, expr string) Schedule {
	t.Helper()
	return mustParse(t, expr).withLocation(time.UTC)
}

// 1. Parse menerima ekspresi sah dan menormalkan spasi pada String.
func TestParseValid(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"* * * * *", "* * * * *"},
		{"0 2 * * *", "0 2 * * *"},
		{"*/15 9-17 * * 1-5", "*/15 9-17 * * 1-5"},
		{"0 0 1,15 * *", "0 0 1,15 * *"},
		{"  0   2 *   * *  ", "0 2 * * *"},
		{"5,10 0-6/2 1-31 1-12 0-6", "5,10 0-6/2 1-31 1-12 0-6"},
	}

	for _, tc := range cases {
		s, err := Parse(tc.expr)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tc.expr, err)
			continue
		}
		if got := s.String(); got != tc.want {
			t.Errorf("Parse(%q).String() = %q, ingin %q", tc.expr, got, tc.want)
		}
	}
}

// 2. Parse menolak ekspresi tidak sah dan menyebut bagian yang bermasalah.
func TestParseInvalid(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{"* * * *", "5 field"},        // kurang satu field
		{"* * * * * *", "5 field"},    // kelebihan field
		{"", "5 field"},               // kosong
		{"60 * * * *", "menit"},       // menit di atas 59
		{"* 24 * * *", "jam"},         // jam di atas 23
		{"* * 0 * *", "hari-bulan"},   // hari-bulan minimal 1
		{"* * * 13 *", "bulan"},       // bulan di atas 12
		{"*/0 * * * *", "langkah"},    // langkah nol
		{"* * * * 8", "hari-minggu"},  // dow di atas 7
		{"0 0 1 1 0,", "hari-minggu"}, // elemen daftar kosong
		{"abc * * * *", "menit"},      // bukan angka
		{"0 5-1 * * *", "jam"},        // rentang terbalik
		{"0 1-2-3 * * *", "jam"},      // rentang bersarang
	}

	for _, tc := range cases {
		s, err := Parse(tc.expr)
		if err == nil {
			t.Errorf("Parse(%q) = %q, ingin error", tc.expr, s.String())
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) error = %q, ingin memuat %q", tc.expr, err.Error(), tc.want)
		}
	}
}

// 3a. Matches memakai resolusi menit dan menghormati hari-minggu.
func TestMatchesDayAndMinute(t *testing.T) {
	s := utc(t, "0 2 * * 1")

	// 1 Januari 2024 adalah hari Senin.
	monday := time.Date(2024, time.January, 1, 2, 0, 0, 0, time.UTC)
	if !s.Matches(monday) {
		t.Errorf("Matches(%s) = false, ingin true untuk jadwal %q", monday, s)
	}

	if s.Matches(monday.Add(time.Minute)) {
		t.Errorf("Matches(%s) = true, ingin false: menit 1 tidak cocok dengan %q", monday.Add(time.Minute), s)
	}
	if s.Matches(monday.Add(24 * time.Hour)) {
		t.Errorf("Matches(%s) = true, ingin false: Selasa tidak cocok dengan %q", monday.Add(24*time.Hour), s)
	}
	if s.Matches(time.Date(2024, time.January, 1, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("Matches(03:00) = true, ingin false untuk jadwal %q", s)
	}

	// Detik diabaikan: 02:00:59 masih menit yang sama.
	if !s.Matches(time.Date(2024, time.January, 1, 2, 0, 59, 0, time.UTC)) {
		t.Errorf("Matches(02:00:59) = false, ingin true: resolusi menit")
	}
}

// 3b. Langkah "*/15" cocok pada menit 0, 15, 30 dan 45 saja.
func TestMatchesStep(t *testing.T) {
	s := utc(t, "*/15 * * * *")
	base := time.Date(2024, time.March, 5, 10, 0, 0, 0, time.UTC) // Selasa

	for _, m := range []int{0, 15, 30, 45} {
		if !s.Matches(base.Add(time.Duration(m) * time.Minute)) {
			t.Errorf("Matches(menit %d) = false, ingin true untuk %q", m, s)
		}
	}
	for _, m := range []int{1, 7, 14, 16, 29, 31, 59} {
		if s.Matches(base.Add(time.Duration(m) * time.Minute)) {
			t.Errorf("Matches(menit %d) = true, ingin false untuk %q", m, s)
		}
	}
}

// 4. Hari-minggu 7 sama dengan 0 (Minggu).
func TestMatchesSundaySeven(t *testing.T) {
	seven := utc(t, "0 2 * * 7")
	zero := utc(t, "0 2 * * 0")

	// 7 Januari 2024 adalah hari Minggu.
	sunday := time.Date(2024, time.January, 7, 2, 0, 0, 0, time.UTC)
	if !seven.Matches(sunday) {
		t.Errorf("Matches(Minggu) = false untuk %q, ingin true", seven)
	}
	if !zero.Matches(sunday) {
		t.Errorf("Matches(Minggu) = false untuk %q, ingin true", zero)
	}

	monday := sunday.Add(24 * time.Hour)
	if seven.Matches(monday) || zero.Matches(monday) {
		t.Errorf("Matches(Senin) = true untuk %q/%q, ingin false", seven, zero)
	}

	// Ekspresi "0-7" harus sama artinya dengan "*".
	all := utc(t, "0 2 * * 0-7")
	for d := 0; d < 7; d++ {
		day := time.Date(2024, time.January, 7+d, 2, 0, 0, 0, time.UTC)
		if !all.Matches(day) {
			t.Errorf("Matches(%s) = false untuk %q, ingin true", day.Weekday(), all)
		}
	}
}

// 3c. Rentang, daftar dan langkah pada rentang dipadukan dengan benar.
func TestMatchesRangesListsAndSteps(t *testing.T) {
	hours := utc(t, "0 9-17 * * *")
	for _, h := range []int{9, 12, 17} {
		at := time.Date(2024, time.May, 6, h, 0, 0, 0, time.UTC)
		if !hours.Matches(at) {
			t.Errorf("Matches(%02d:00) = false untuk %q, ingin true", h, hours)
		}
	}
	for _, h := range []int{8, 18, 23} {
		at := time.Date(2024, time.May, 6, h, 0, 0, 0, time.UTC)
		if hours.Matches(at) {
			t.Errorf("Matches(%02d:00) = true untuk %q, ingin false", h, hours)
		}
	}

	list := utc(t, "0 0 1,15 * *")
	for _, d := range []int{1, 15} {
		if !list.Matches(time.Date(2024, time.June, d, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("Matches(tanggal %d) = false untuk %q, ingin true", d, list)
		}
	}
	if list.Matches(time.Date(2024, time.June, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Matches(tanggal 2) = true untuk %q, ingin false", list)
	}

	stepped := utc(t, "0 0-23/6 * * *")
	for _, h := range []int{0, 6, 12, 18} {
		at := time.Date(2024, time.May, 6, h, 0, 0, 0, time.UTC)
		if !stepped.Matches(at) {
			t.Errorf("Matches(%02d:00) = false untuk %q, ingin true", h, stepped)
		}
	}
	for _, h := range []int{1, 5, 7, 23} {
		at := time.Date(2024, time.May, 6, h, 0, 0, 0, time.UTC)
		if stepped.Matches(at) {
			t.Errorf("Matches(%02d:00) = true untuk %q, ingin false", h, stepped)
		}
	}
}

// Matches memakai zona waktu Schedule: 02:00 UTC adalah 09:00 di Jakarta.
func TestMatchesUsesScheduleLocation(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Skipf("zona Asia/Jakarta tidak tersedia: %v", err)
	}

	s := mustParse(t, "0 2 * * *").withLocation(time.UTC)
	at := time.Date(2024, time.March, 5, 2, 0, 0, 0, time.UTC)

	if !s.Matches(at) {
		t.Fatalf("Matches(%s) = false untuk %q, ingin true", at, s)
	}

	// 02:00 UTC = 09:00 Jakarta, jadi jadwal yang sama tidak cocok bila
	// dievaluasi pada zona Jakarta.
	local := s.withLocation(jakarta)
	if local.Matches(at) {
		t.Errorf("Matches(%s) = true pada zona Jakarta untuk %q, ingin false", at, local)
	}

	// 02:00 Jakarta = 19:00 UTC hari sebelumnya.
	jakartaTwo := at.Add(-7 * time.Hour)
	if !local.Matches(jakartaTwo) {
		t.Errorf("Matches(%s) = false pada zona Jakarta untuk %q, ingin true", jakartaTwo, local)
	}
	if s.withLocation(time.UTC).Matches(jakartaTwo) {
		t.Errorf("Matches(%s) = true pada zona UTC untuk %q, ingin false", jakartaTwo, s)
	}
}

// Schedule nol tidak cocok dengan waktu mana pun.
func TestZeroScheduleNeverMatches(t *testing.T) {
	var zero Schedule
	if zero.Matches(time.Now()) {
		t.Error("Schedule nol cocok dengan waktu, ingin tidak pernah cocok")
	}
	if got := zero.String(); got != "" {
		t.Errorf("String() Schedule nol = %q, ingin kosong", got)
	}
}
