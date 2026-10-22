package ingest

import (
	"fmt"

	"vodlocal/internal/appsettings"
	"vodlocal/internal/hls"
)

// defaultMaxShortSide adalah batas fallback untuk test dan pemanggil lama.
const defaultMaxShortSide = appsettings.DefaultMaxShortSide

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

// selectVariant adalah wrapper kompatibilitas test; pemilihan nyata memakai
// hls.SelectVariant dengan batas dari setting run.
func selectVariant(variants []hls.Variant) (hls.Variant, bool) {
	return hls.SelectVariant(variants, defaultMaxShortSide)
}
