package ingest

import (
	"testing"

	"vodlocal/internal/hls"
)

// variant membangun satu hls.Variant dengan URI yang selalu terisi.
func variant(width, height, bandwidth int, uri string) hls.Variant {
	return hls.Variant{URI: uri, Width: width, Height: height, Bandwidth: bandwidth}
}

// TestShortSide memastikan sisi pendek memakai min(width,height).
func TestShortSide(t *testing.T) {
	cases := []struct {
		width, height int
		want          int
	}{
		{640, 360, 360},
		{540, 960, 540},
		{1920, 1080, 1080},
		{0, 0, 0},
		{1280, 0, 0},
		{0, 720, 0},
	}

	for _, c := range cases {
		if got := shortSide(c.width, c.height); got != c.want {
			t.Errorf("shortSide(%d,%d) = %d, mau %d", c.width, c.height, got, c.want)
		}
	}
}

// TestQualityLabel memastikan label kualitas mengikuti skema "360p", "420k" dan
// "q<bandwidth>".
func TestQualityLabel(t *testing.T) {
	cases := []struct {
		width, height, bandwidth int
		want                     string
	}{
		{640, 360, 800000, "360p"},
		{360, 640, 800000, "360p"},
		{540, 960, 1500000, "540p"},
		{854, 480, 1200000, "480p"},
		{0, 0, 420000, "420k"},
		{0, 0, 1500000, "1500k"},
		{0, 0, 999, "q999"},
		{0, 0, 0, "q0"},
		{1280, 0, 800000, "800k"},
	}

	for _, c := range cases {
		if got := qualityLabel(c.width, c.height, c.bandwidth); got != c.want {
			t.Errorf("qualityLabel(%d,%d,%d) = %q, mau %q",
				c.width, c.height, c.bandwidth, got, c.want)
		}
	}
}

// TestSelectVariant memastikan aturan pemilihan varian: sisi pendek <= 480 dan
// terbesar, lalu varian terkecil bila tidak ada yang memenuhi.
func TestSelectVariant(t *testing.T) {
	cases := []struct {
		name     string
		variants []hls.Variant
		wantURI  string
		wantOK   bool
	}{
		{
			name: "campuran memilih yang terbesar di bawah 480",
			variants: []hls.Variant{
				variant(1920, 1080, 4000000, "hd.m3u8"),
				variant(540, 960, 1500000, "portrait.m3u8"),
				variant(640, 360, 800000, "sd.m3u8"),
				variant(854, 480, 1200000, "medium.m3u8"),
				variant(1280, 720, 2200000, "720.m3u8"),
			},
			wantURI: "medium.m3u8",
			wantOK:  true,
		},
		{
			name: "hanya varian besar memilih yang terkecil",
			variants: []hls.Variant{
				variant(1920, 1080, 4000000, "full-hd.m3u8"),
				variant(1280, 720, 2200000, "720.m3u8"),
				variant(1080, 1920, 3500000, "portrait-hd.m3u8"),
			},
			wantURI: "720.m3u8",
			wantOK:  true,
		},
		{
			name: "tanpa resolusi memilih bandwidth terbesar",
			variants: []hls.Variant{
				variant(0, 0, 420000, "low.m3u8"),
				variant(0, 0, 900000, "high.m3u8"),
			},
			wantURI: "high.m3u8",
			wantOK:  true,
		},
		{
			name: "resolusi dikenal diutamakan atas yang tanpa resolusi",
			variants: []hls.Variant{
				variant(0, 0, 900000, "tanpa-resolusi.m3u8"),
				variant(640, 360, 800000, "sd.m3u8"),
			},
			wantURI: "sd.m3u8",
			wantOK:  true,
		},
		{
			name: "sisi pendek 480 masih diterima",
			variants: []hls.Variant{
				variant(854, 480, 1200000, "480.m3u8"),
				variant(640, 360, 800000, "360.m3u8"),
			},
			wantURI: "480.m3u8",
			wantOK:  true,
		},
		{
			name: "bandwidth sama memilih sisi pendek lebih besar",
			variants: []hls.Variant{
				variant(360, 640, 800000, "kecil.m3u8"),
				variant(480, 854, 800000, "besar.m3u8"),
			},
			wantURI: "besar.m3u8",
			wantOK:  true,
		},
		{
			name:     "tanpa varian",
			variants: nil,
			wantOK:   false,
		},
		{
			name: "semua varian tanpa URI",
			variants: []hls.Variant{
				{Width: 640, Height: 360, Bandwidth: 800000},
				{URI: "   ", Width: 640, Height: 360},
			},
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := selectVariant(c.variants)
			if ok != c.wantOK {
				t.Fatalf("selectVariant ok = %v, mau %v", ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if got.URI != c.wantURI {
				t.Errorf("selectVariant = %q, mau %q", got.URI, c.wantURI)
			}
		})
	}
}

// TestSelectVariantFallbackPrefersSmallest memastikan fallback memakai bandwidth
// terkecil ketika semua varian besar dan beresolusi sama-sama tidak diketahui.
func TestSelectVariantFallbackPrefersSmallest(t *testing.T) {
	got, ok := selectVariant([]hls.Variant{
		variant(1080, 1920, 3500000, "portrait-hd.m3u8"),
		variant(1280, 720, 2200000, "720.m3u8"),
	})
	if !ok {
		t.Fatal("selectVariant ok = false, mau true")
	}
	if got.URI != "720.m3u8" {
		t.Errorf("selectVariant = %q, mau 720.m3u8", got.URI)
	}

	if label := qualityLabel(got.Width, got.Height, got.Bandwidth); label != "720p" {
		t.Errorf("qualityLabel = %q, mau 720p", label)
	}
}
