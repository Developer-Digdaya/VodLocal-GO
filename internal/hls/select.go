package hls

import "strings"

// SelectVariant memilih satu varian master playlist berdasarkan batas sisi
// pendek:
//   - kandidat adalah varian ber-URI dengan sisi pendek <= maxShortSide;
//   - kandidat terbesar (sisi pendek, lalu bandwidth) dipilih;
//   - bila tidak ada kandidat, varian terkecil dipakai sebagai fallback.
func SelectVariant(variants []Variant, maxShortSide int) (Variant, bool) {
	var (
		best  Variant
		found bool
	)

	for _, v := range variants {
		if strings.TrimSpace(v.URI) == "" {
			continue
		}
		if variantShortSide(v) > maxShortSide {
			continue
		}
		if !found || largerVariant(v, best) {
			best, found = v, true
		}
	}
	if found {
		return best, true
	}

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

func variantShortSide(v Variant) int {
	if v.Width <= 0 || v.Height <= 0 {
		return 0
	}
	if v.Width < v.Height {
		return v.Width
	}
	return v.Height
}

func largerVariant(a, b Variant) bool {
	if as, bs := variantShortSide(a), variantShortSide(b); as != bs {
		return as > bs
	}
	if a.Bandwidth != b.Bandwidth {
		return a.Bandwidth > b.Bandwidth
	}
	return a.AverageBandwidth > b.AverageBandwidth
}

func smallerVariant(a, b Variant) bool {
	if as, bs := variantShortSide(a), variantShortSide(b); as != bs {
		return as < bs
	}
	if a.Bandwidth != b.Bandwidth {
		return a.Bandwidth < b.Bandwidth
	}
	return a.AverageBandwidth < b.AverageBandwidth
}
