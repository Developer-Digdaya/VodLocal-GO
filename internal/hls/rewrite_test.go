package hls

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Test 7: segment URIs are replaced by deterministic local names.
func TestRewriteSegments(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXTINF:9.009,
http://origin.example.com/live/first.ts
#EXTINF:9.009,
/full/path/second.m4s?token=abc
#EXTINF:3.003,
third
#EXT-X-ENDLIST
`
	p := mustParse(t, raw)
	out, assets, err := Rewrite(p, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	wantAssets := []AssetRef{
		{Kind: KindSegment, SourceURI: "http://origin.example.com/live/first.ts", LocalName: "seg-00001.ts"},
		{Kind: KindSegment, SourceURI: "/full/path/second.m4s?token=abc", LocalName: "seg-00002.m4s"},
		{Kind: KindSegment, SourceURI: "third", LocalName: "seg-00003.ts"},
	}
	if !reflect.DeepEqual(assets, wantAssets) {
		t.Errorf("assets = %+v, want %+v", assets, wantAssets)
	}

	got := string(out)
	for _, want := range []string{
		"#EXTINF:9.009,\nseg-00001.ts\n",
		"#EXTINF:9.009,\nseg-00002.m4s\n",
		"#EXTINF:3.003,\nseg-00003.ts\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"first.ts", "second.m4s", "third", "origin.example.com"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output still references the source URI %q:\n%s", unwanted, got)
		}
	}

	// The header and the tags must survive unchanged.
	for _, want := range []string{"#EXTM3U\n", "#EXT-X-VERSION:3\n", "#EXT-X-TARGETDURATION:10\n", "#EXT-X-MEDIA-SEQUENCE:0\n", "#EXT-X-ENDLIST\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q:\n%s", want, got)
		}
	}
}

// Test 8: key and map URIs get their own local names.
func TestRewriteKeyAndMap(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="https://origin.example.com/init.mp4",BYTERANGE="720@0"
#EXT-X-KEY:METHOD=AES-128,URI="https://kms.example.com/k1.bin",IV=0x00112233445566778899aabbccddeeff
#EXTINF:4.0,
s1.m4s
#EXTINF:4.0,
s2.m4s
#EXT-X-ENDLIST
`
	p := mustParse(t, raw)
	out, assets, err := Rewrite(p, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	wantAssets := []AssetRef{
		{Kind: KindMap, SourceURI: "https://origin.example.com/init.mp4", LocalName: "init-01.mp4"},
		{Kind: KindKey, SourceURI: "https://kms.example.com/k1.bin", LocalName: "key-01.key"},
		{Kind: KindSegment, SourceURI: "s1.m4s", LocalName: "seg-00001.m4s"},
		{Kind: KindSegment, SourceURI: "s2.m4s", LocalName: "seg-00002.m4s"},
	}
	if !reflect.DeepEqual(assets, wantAssets) {
		t.Errorf("assets = %+v, want %+v", assets, wantAssets)
	}

	got := string(out)
	for _, want := range []string{
		`#EXT-X-MAP:URI="init-01.mp4",BYTERANGE="720@0"`,
		`#EXT-X-KEY:METHOD=AES-128,URI="key-01.key",IV=0x00112233445566778899aabbccddeeff`,
		"#EXTINF:4.0,\nseg-00001.m4s\n#EXTINF:4.0,\nseg-00002.m4s\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not contain %q:\n%s", want, got)
		}
	}
}

// Test that two keys and two maps are numbered independently and that a key
// reused by several segments keeps a single name.
func TestRewriteMultipleKeysAndMaps(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init-a.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="k1.bin"
#EXTINF:4.0,
a1.m4s
#EXTINF:4.0,
a2.m4s
#EXT-X-KEY:METHOD=AES-128,URI="k2.bin"
#EXT-X-MAP:URI="init-b.mp4"
#EXTINF:4.0,
b1.m4s
#EXT-X-ENDLIST
`
	p := mustParse(t, raw)
	out, assets, err := Rewrite(p, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	wantAssets := []AssetRef{
		{Kind: KindMap, SourceURI: "init-a.mp4", LocalName: "init-01.mp4"},
		{Kind: KindKey, SourceURI: "k1.bin", LocalName: "key-01.key"},
		{Kind: KindSegment, SourceURI: "a1.m4s", LocalName: "seg-00001.m4s"},
		{Kind: KindSegment, SourceURI: "a2.m4s", LocalName: "seg-00002.m4s"},
		{Kind: KindKey, SourceURI: "k2.bin", LocalName: "key-02.key"},
		{Kind: KindMap, SourceURI: "init-b.mp4", LocalName: "init-02.mp4"},
		{Kind: KindSegment, SourceURI: "b1.m4s", LocalName: "seg-00003.m4s"},
	}
	if !reflect.DeepEqual(assets, wantAssets) {
		t.Errorf("assets = %+v, want %+v", assets, wantAssets)
	}

	got := string(out)
	if strings.Count(got, `URI="key-01.key"`) != 1 {
		t.Errorf("key-01.key should be written once:\n%s", got)
	}
	if !strings.Contains(got, `URI="key-02.key"`) || !strings.Contains(got, `URI="init-02.mp4"`) {
		t.Errorf("second key/map names missing:\n%s", got)
	}
}

// Test 9: rewritten playlists parse back with the same segments and durations.
func TestRewriteRoundTrip(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:17
#EXT-X-PLAYLIST-TYPE:EVENT
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="https://kms.example.com/k1.bin",IV=0x0102030405060708090a0b0c0d0e0f10
#EXTINF:6.006,first
seg0.m4s
#EXTINF:6.006,
seg1.m4s
#EXT-X-DISCONTINUITY
#EXT-X-PROGRAM-DATE-TIME:2024-05-01T10:00:00Z
#EXTINF:6.006,
seg2.m4s?token=xyz
#EXT-X-ENDLIST
`
	orig := mustParse(t, raw)

	out, _, err := Rewrite(orig, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Errorf("output is not newline terminated: %q", out)
	}

	got, err := Parse(strings.NewReader(string(out)))
	if err != nil {
		t.Fatalf("Parse(rewritten) error = %v\n%s", err, out)
	}
	if len(got.Segments) != len(orig.Segments) {
		t.Fatalf("len(Segments) = %d, want %d\n%s", len(got.Segments), len(orig.Segments), out)
	}
	for i := range orig.Segments {
		if !almostEqual(got.Segments[i].Duration, orig.Segments[i].Duration) {
			t.Errorf("segments[%d].Duration = %v, want %v", i, got.Segments[i].Duration, orig.Segments[i].Duration)
		}
		if got.Segments[i].Key == nil {
			t.Errorf("segments[%d] lost its key", i)
		}
		if got.Segments[i].Map == nil {
			t.Errorf("segments[%d] lost its map", i)
		}
	}
	if got.MediaSequence != orig.MediaSequence {
		t.Errorf("MediaSequence = %d, want %d", got.MediaSequence, orig.MediaSequence)
	}
	if got.TargetDuration != orig.TargetDuration {
		t.Errorf("TargetDuration = %d, want %d", got.TargetDuration, orig.TargetDuration)
	}
	if got.IsMaster {
		t.Errorf("IsMaster = true, want false")
	}
	// Tags the parser does not model are preserved.
	for _, want := range []string{"#EXT-X-DISCONTINUITY", "#EXT-X-PROGRAM-DATE-TIME:2024-05-01T10:00:00Z", "#EXT-X-PLAYLIST-TYPE:EVENT"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
}

// Test that a media playlist carrying a variant declaration is cleaned up: the
// #EXT-X-STREAM-INF tag and its URI are dropped, the rest is kept.
func TestRewriteDropsVariantLines(t *testing.T) {
	p := &Playlist{
		Version:        3,
		TargetDuration: 10,
		Segments:       []Segment{{URI: "a.ts", Duration: 10}},
		Raw: "#EXTM3U\n" +
			"#EXT-X-VERSION:3\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=640x360\n" +
			"low/index.m3u8\n" +
			"#EXTINF:10,\n" +
			"a.ts\n",
	}
	out, assets, err := Rewrite(p, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	got := string(out)
	if strings.Contains(got, "#EXT-X-STREAM-INF") || strings.Contains(got, "low/index.m3u8") {
		t.Errorf("variant lines were not dropped:\n%s", got)
	}
	if !strings.Contains(got, "seg-00001.ts\n") {
		t.Errorf("segment URI was not rewritten:\n%s", got)
	}
	wantAssets := []AssetRef{{Kind: KindSegment, SourceURI: "a.ts", LocalName: "seg-00001.ts"}}
	if !reflect.DeepEqual(assets, wantAssets) {
		t.Errorf("assets = %+v, want %+v", assets, wantAssets)
	}

	if _, err := Parse(strings.NewReader(got)); err != nil {
		t.Errorf("Parse(rewritten) error = %v\n%s", err, got)
	}
}

// Test that a playlist built in memory (no Raw) is rendered from its segments.
func TestRewriteWithoutRaw(t *testing.T) {
	p := &Playlist{
		Version:        3,
		TargetDuration: 10,
		MediaSequence:  5,
		Segments: []Segment{
			{URI: "a.ts", Duration: 9.5},
			{URI: "b.aac", Duration: 10, Key: &Key{Method: "AES-128", URI: "https://kms/k.bin"}},
		},
	}
	out, assets, err := Rewrite(p, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}

	// Assets follow the order of the rendered document: the key tag is written
	// just before the segment it applies to.
	wantAssets := []AssetRef{
		{Kind: KindSegment, SourceURI: "a.ts", LocalName: "seg-00001.ts"},
		{Kind: KindKey, SourceURI: "https://kms/k.bin", LocalName: "key-01.key"},
		{Kind: KindSegment, SourceURI: "b.aac", LocalName: "seg-00002.aac"},
	}
	if !reflect.DeepEqual(assets, wantAssets) {
		t.Errorf("assets = %+v, want %+v", assets, wantAssets)
	}

	got := string(out)
	if !strings.HasPrefix(got, "#EXTM3U\n") {
		t.Errorf("output does not start with #EXTM3U:\n%s", got)
	}
	if !strings.Contains(got, `URI="key-01.key"`) || !strings.Contains(got, "#EXT-X-ENDLIST") {
		t.Errorf("output is incomplete:\n%s", got)
	}

	reparsed, err := Parse(strings.NewReader(got))
	if err != nil {
		t.Fatalf("Parse(rewritten) error = %v\n%s", err, got)
	}
	if len(reparsed.Segments) != 2 {
		t.Errorf("len(Segments) = %d, want 2", len(reparsed.Segments))
	}
}

// Test 10: master playlists cannot be rewritten.
func TestRewriteMasterPlaylist(t *testing.T) {
	p := mustParse(t, `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1280000
low/index.m3u8
`)
	out, assets, err := Rewrite(p, RewriteOptions{})
	if !errors.Is(err, ErrMasterPlaylist) {
		t.Fatalf("Rewrite() error = %v, want ErrMasterPlaylist", err)
	}
	if out != nil || assets != nil {
		t.Errorf("Rewrite() returned %q / %+v on error, want nil", out, assets)
	}
}

// Test the guards around Rewrite.
func TestRewriteGuards(t *testing.T) {
	if _, _, err := Rewrite(nil, RewriteOptions{}); err == nil {
		t.Errorf("Rewrite(nil) error = nil, want an error")
	}
	if _, _, err := Rewrite(&Playlist{}, RewriteOptions{}); !errors.Is(err, ErrNoSegments) {
		t.Errorf("Rewrite(empty) error = %v, want ErrNoSegments", err)
	}
}

// Test 11: BuildMaster renders a master playlist for the given variants.
func TestBuildMaster(t *testing.T) {
	variants := []Variant{
		{URI: "low/index.m3u8", Bandwidth: 1280000, AverageBandwidth: 1000000, Width: 640, Height: 360, Codecs: "avc1.4d401e,mp4a.40.2"},
		{URI: "high/index.m3u8", Bandwidth: 2560000},
	}

	got := string(BuildMaster(variants))
	want := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-INDEPENDENT-SEGMENTS\n" +
		"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1280000,AVERAGE-BANDWIDTH=1000000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\"\n" +
		"low/index.m3u8\n" +
		"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2560000\n" +
		"high/index.m3u8\n"
	if got != want {
		t.Errorf("BuildMaster() =\n%s\nwant:\n%s", got, want)
	}

	// The result must be a valid master playlist with the variant URIs in order.
	p, err := Parse(strings.NewReader(got))
	if err != nil {
		t.Fatalf("Parse(BuildMaster()) error = %v", err)
	}
	if !p.IsMaster {
		t.Fatalf("IsMaster = false, want true")
	}
	if len(p.Variants) != 2 {
		t.Fatalf("len(Variants) = %d, want 2", len(p.Variants))
	}
	if p.Variants[0].URI != "low/index.m3u8" || p.Variants[1].URI != "high/index.m3u8" {
		t.Errorf("variant order = %q, %q", p.Variants[0].URI, p.Variants[1].URI)
	}
	if p.Variants[0].Width != 640 || p.Variants[0].Height != 360 {
		t.Errorf("variants[0] resolution = %dx%d, want 640x360", p.Variants[0].Width, p.Variants[0].Height)
	}
	if p.Variants[0].Codecs != "avc1.4d401e,mp4a.40.2" {
		t.Errorf("variants[0].Codecs = %q", p.Variants[0].Codecs)
	}
	if p.Variants[1].AverageBandwidth != 0 || p.Variants[1].Width != 0 {
		t.Errorf("variants[1] = %+v, want omitted optional attributes", p.Variants[1])
	}
}

// Test that BuildMaster without variants emits only the header.
func TestBuildMasterEmpty(t *testing.T) {
	got := string(BuildMaster(nil))
	want := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n"
	if got != want {
		t.Errorf("BuildMaster(nil) = %q, want %q", got, want)
	}
	if strings.Contains(got, "#EXT-X-STREAM-INF") {
		t.Errorf("BuildMaster(nil) emitted a variant:\n%s", got)
	}
}

// Test the extension helper used to name local files.
func TestExtOf(t *testing.T) {
	cases := []struct{ uri, want string }{
		{"a/b/seg.ts", "ts"},
		{"seg.m4s?token=1", "m4s"},
		{"seg.aac#frag", "aac"},
		{"seg", "ts"},
		{"seg.", "ts"},
		{"http://h/p/SEG.TS", "ts"},
		{"weird.ext with space", "ts"},
	}
	for _, c := range cases {
		if got := extOf(c.uri, "ts"); got != c.want {
			t.Errorf("extOf(%q) = %q, want %q", c.uri, got, c.want)
		}
	}
}

// Key yang tertanam sebagai data URI harus dipertahankan dan tidak dianggap
// sebagai aset yang perlu diunduh.
func TestRewriteKeepsDataURIKey(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-KEY:METHOD=AES-128,URI="data:text/plain;base64,AAAA",IV=0x1234
#EXT-X-TARGETDURATION:4
#EXTINF:4.0,
seg0.ts
#EXT-X-ENDLIST
`

	pl, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, assets, err := Rewrite(pl, RewriteOptions{})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}

	if !strings.Contains(string(out), `URI="data:text/plain;base64,AAAA"`) {
		t.Errorf("baris key data URI berubah:\n%s", out)
	}
	for _, a := range assets {
		if a.Kind == KindKey {
			t.Errorf("key data URI tidak boleh menjadi aset: %+v", a)
		}
	}
	if len(assets) != 1 || assets[0].Kind != KindSegment {
		t.Fatalf("aset = %+v, ingin satu segmen", assets)
	}
}
