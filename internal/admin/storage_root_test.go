package admin

import "testing"

// TestSeriesRootFromPlaylist membedakan layout series (5 level) dan movie
// (4 level) agar hapus per konten tidak menghapus kategori movie sekaligus.
func TestSeriesRootFromPlaylist(t *testing.T) {
	series := "dramahits/series/9d1c-4f5a/ep-03/360p/index.m3u8"
	if got, want := seriesRootFromPlaylist(series, false), "dramahits/series/9d1c-4f5a"; got != want {
		t.Fatalf("series root = %q, mau %q", got, want)
	}

	movie := "densgo/movie/10129/360p/index.m3u8"
	if got, want := seriesRootFromPlaylist(movie, true), "densgo/movie/10129"; got != want {
		t.Fatalf("movie root = %q, mau %q", got, want)
	}
}
