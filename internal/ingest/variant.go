package ingest

import (
	"fmt"
	"strings"

	"vodlocal/internal/hls"
)

// maxShortSide adalah batas sisi pendek variant yang masih dianggap layak
// untuk perangkat tablet (720p). Kandidat terbesar di bawah batas ini dipilih
// supaya kualitas gambar setinggi mungkin tanpa membebani jaringan.
const maxShortSide = 720

// shortSide mengembalikan sisi pendek sebuah resolusi (min(width,height)), atau
// 0 bila resolusinya tidak diketahui.
//
// Sisi pendek dipakai sebagai ukuran "kualitas" karena satu series bisa
// disajikan dalam orientasi portrait (540x960) maupun landscape (640x360);
// perbandingan lebar saja akan salah menilai keduanya.
func shortSide(width, height int) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	return min(width, height)
}

// hasResolution melaporkan apakah kedua sisi resolusi diketahui.
func hasResolution(width, height int) bool {
	return width > 0 && height > 0
}

// qualityLabel menyusun label kualitas yang dipakai sebagai nama kolom
// renditions.quality dan komponen path di dalam generation.
//
// Aturannya:
//
//   - resolusi diketahui  -> "<sisi pendek>p", mis. "360p",
//   - resolusi kosong     -> "<bandwidth/1000>k", mis. "420k",
//   - bandwidth < 1000    -> "q<bandwidth>", mis. "q999" (atau "q0" bila
//     bandwidth juga tidak diketahui).
//
// Label selalu berupa satu komponen path yang aman (huruf, angka, dan akhiran
// "p"/"k"), sehingga bisa langsung dipakai di bawah direktori generation.
func qualityLabel(width, height, bandwidth int) string {
	if hasResolution(width, height) {
		return fmt.Sprintf("%dp", shortSide(width, height))
	}
	if bandwidth >= 1000 {
		return fmt.Sprintf("%dk", bandwidth/1000)
	}
	return fmt.Sprintf("q%d", bandwidth)
}

// selectVariant memilih satu variant dari master playlist:
//
//   - semua variant dengan sisi pendek <= maxShortSide menjadi kandidat, lalu
//     yang terbesar (sisi pendek, lalu bandwidth) dipilih;
//   - bila tidak ada kandidat, variant terkecil (sisi pendek, lalu bandwidth)
//     dipakai sebagai jalan terakhir.
//
// Variant tanpa URI dilewati karena tidak bisa diunduh. Hasil kedua bernilai
// false bila tidak ada variant yang bisa dipakai.
func selectVariant(variants []hls.Variant) (hls.Variant, bool) {
	var (
		best  hls.Variant
		found bool
	)

	for _, v := range variants {
		if strings.TrimSpace(v.URI) == "" {
			continue
		}
		if shortSide(v.Width, v.Height) > maxShortSide {
			continue
		}
		if !found || preferVariant(v, best) {
			best, found = v, true
		}
	}
	if found {
		return best, true
	}

	// Tidak ada variant yang cukup kecil: pakai yang paling ringan supaya
	// unduhan tetap selesai dalam waktu wajar.
	for _, v := range variants {
		if strings.TrimSpace(v.URI) == "" {
			continue
		}
		if !found || smallerVariant(v, best) {
			best, found = v, true
		}
	}
	return best, found
}

// preferVariant melaporkan apakah a lebih layak daripada b di dalam kandidat
// yang sudah dibatasi maxShortSide: sisi pendek lebih besar diutamakan, lalu
// bandwidth lebih besar.
func preferVariant(a, b hls.Variant) bool {
	if as, bs := shortSide(a.Width, a.Height), shortSide(b.Width, b.Height); as != bs {
		return as > bs
	}
	if a.Bandwidth != b.Bandwidth {
		return a.Bandwidth > b.Bandwidth
	}
	return a.AverageBandwidth > b.AverageBandwidth
}

// smallerVariant melaporkan apakah a lebih ringan daripada b: sisi pendek lebih
// kecil diutamakan, lalu bandwidth lebih kecil.
func smallerVariant(a, b hls.Variant) bool {
	if as, bs := shortSide(a.Width, a.Height), shortSide(b.Width, b.Height); as != bs {
		return as < bs
	}
	if a.Bandwidth != b.Bandwidth {
		return a.Bandwidth < b.Bandwidth
	}
	return a.AverageBandwidth < b.AverageBandwidth
}
