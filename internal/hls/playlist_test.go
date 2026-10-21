package hls

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// mustParse parses a playlist or fails the test.
func mustParse(t *testing.T, raw string) *Playlist {
	t.Helper()
	p, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return p
}

// almostEqual compares playlist durations, which are decimal in the source.
func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// simpleMedia is a three segment media playlist without encryption.
const simpleMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:42
#EXTINF:9.009,
first.ts
#EXTINF:9.009,Second segment
https://cdn.example.com/path/second.ts
#EXTINF:3.003,
third.ts
#EXT-X-ENDLIST
`

// Test 1: a plain media playlist yields durations, URIs and sequences.
func TestParseMediaPlaylist(t *testing.T) {
	p := mustParse(t, simpleMedia)

	if p.IsMaster {
		t.Fatalf("IsMaster = true, want false")
	}
	if p.Version != 3 {
		t.Errorf("Version = %d, want 3", p.Version)
	}
	if p.TargetDuration != 10 {
		t.Errorf("TargetDuration = %d, want 10", p.TargetDuration)
	}
	if p.MediaSequence != 42 {
		t.Errorf("MediaSequence = %d, want 42", p.MediaSequence)
	}
	if len(p.Variants) != 0 {
		t.Errorf("len(Variants) = %d, want 0", len(p.Variants))
	}
	if len(p.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(p.Segments))
	}

	want := []struct {
		uri      string
		duration float64
		title    string
		sequence int
	}{
		{"first.ts", 9.009, "", 42},
		{"https://cdn.example.com/path/second.ts", 9.009, "Second segment", 43},
		{"third.ts", 3.003, "", 44},
	}
	for i, w := range want {
		got := p.Segments[i]
		if got.URI != w.uri {
			t.Errorf("segments[%d].URI = %q, want %q", i, got.URI, w.uri)
		}
		if !almostEqual(got.Duration, w.duration) {
			t.Errorf("segments[%d].Duration = %v, want %v", i, got.Duration, w.duration)
		}
		if got.Title != w.title {
			t.Errorf("segments[%d].Title = %q, want %q", i, got.Title, w.title)
		}
		if got.Sequence != w.sequence {
			t.Errorf("segments[%d].Sequence = %d, want %d", i, got.Sequence, w.sequence)
		}
		if got.Key != nil {
			t.Errorf("segments[%d].Key = %+v, want nil", i, got.Key)
		}
		if got.Map != nil {
			t.Errorf("segments[%d].Map = %+v, want nil", i, got.Map)
		}
		if got.ByteRange != nil {
			t.Errorf("segments[%d].ByteRange = %+v, want nil", i, got.ByteRange)
		}
	}

	if p.Raw != simpleMedia {
		t.Errorf("Raw was not preserved verbatim")
	}
}

// Test that CRLF line endings parse exactly like LF.
func TestParseCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(simpleMedia, "\n", "\r\n")
	p := mustParse(t, crlf)

	if len(p.Segments) != 3 {
		t.Fatalf("len(Segments) = %d, want 3", len(p.Segments))
	}
	if p.Segments[1].URI != "https://cdn.example.com/path/second.ts" {
		t.Errorf("segments[1].URI = %q", p.Segments[1].URI)
	}
	if p.Segments[2].Title != "" || !almostEqual(p.Segments[2].Duration, 3.003) {
		t.Errorf("segments[2] = %+v, want duration 3.003 without title", p.Segments[2])
	}
	if p.TargetDuration != 10 || p.MediaSequence != 42 {
		t.Errorf("TargetDuration/MediaSequence = %d/%d, want 10/42", p.TargetDuration, p.MediaSequence)
	}
}

// Test 2: #EXT-X-KEY and #EXT-X-MAP apply to the segments that follow them.
func TestParseKeyAndMap(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:4
#EXT-X-MAP:URI="init.mp4",BYTERANGE="720@0"
#EXTINF:4.0,
s0.m4s
#EXT-X-KEY:METHOD=AES-128,URI="https://kms.example.com/k1.bin",IV=0x00112233445566778899aabbccddeeff,KEYFORMAT="identity",KEYFORMATVERSIONS="1"
#EXTINF:4.0,
s1.m4s
#EXTINF:4.0,
s2.m4s
#EXT-X-KEY:METHOD=NONE
#EXTINF:4.0,
s3.m4s
`
	p := mustParse(t, raw)
	if len(p.Segments) != 4 {
		t.Fatalf("len(Segments) = %d, want 4", len(p.Segments))
	}

	// The map precedes every segment, so all of them reference it.
	for i, seg := range p.Segments {
		if seg.Map == nil {
			t.Fatalf("segments[%d].Map = nil, want the initialization section", i)
		}
		if seg.Map.URI != "init.mp4" {
			t.Errorf("segments[%d].Map.URI = %q, want %q", i, seg.Map.URI, "init.mp4")
		}
		if seg.Map.ByteRange == nil || seg.Map.ByteRange.Length != 720 || seg.Map.ByteRange.Offset != 0 {
			t.Errorf("segments[%d].Map.ByteRange = %+v, want 720@0", i, seg.Map.ByteRange)
		}
	}

	if p.Segments[0].Key != nil {
		t.Errorf("segments[0].Key = %+v, want nil (key tag comes after)", p.Segments[0].Key)
	}

	for _, i := range []int{1, 2} {
		k := p.Segments[i].Key
		if k == nil {
			t.Fatalf("segments[%d].Key = nil, want AES-128 key", i)
		}
		if k.Method != "AES-128" {
			t.Errorf("segments[%d].Key.Method = %q, want AES-128", i, k.Method)
		}
		if k.URI != "https://kms.example.com/k1.bin" {
			t.Errorf("segments[%d].Key.URI = %q", i, k.URI)
		}
		if k.IV != "0x00112233445566778899aabbccddeeff" {
			t.Errorf("segments[%d].Key.IV = %q", i, k.IV)
		}
		if k.KeyFormat != "identity" || k.KeyFormatVersions != "1" {
			t.Errorf("segments[%d].Key format = %q/%q, want identity/1", i, k.KeyFormat, k.KeyFormatVersions)
		}
	}

	// METHOD=NONE clears the key for the following segment.
	if p.Segments[3].Key != nil {
		t.Errorf("segments[3].Key = %+v, want nil after METHOD=NONE", p.Segments[3].Key)
	}
}

// Test 3: #EXT-X-BYTERANGE with and without an explicit offset.
func TestParseByteRange(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:4
#EXT-X-TARGETDURATION:10
#EXTINF:10,
#EXT-X-BYTERANGE:75232@0
video.ts
#EXTINF:10,
#EXT-X-BYTERANGE:82112
video.ts
#EXTINF:10,
#EXT-X-BYTERANGE:69864
video.ts
#EXTINF:10,
plain.ts
#EXT-X-ENDLIST
`
	p := mustParse(t, raw)
	if len(p.Segments) != 4 {
		t.Fatalf("len(Segments) = %d, want 4", len(p.Segments))
	}

	want := []ByteRange{
		{Length: 75232, Offset: 0},
		{Length: 82112, Offset: 75232},
		{Length: 69864, Offset: 157344},
	}
	for i, w := range want {
		got := p.Segments[i].ByteRange
		if got == nil {
			t.Fatalf("segments[%d].ByteRange = nil, want %+v", i, w)
		}
		if *got != w {
			t.Errorf("segments[%d].ByteRange = %+v, want %+v", i, *got, w)
		}
	}
	if p.Segments[3].ByteRange != nil {
		t.Errorf("segments[3].ByteRange = %+v, want nil", p.Segments[3].ByteRange)
	}
}

// Test 4: a master playlist yields its variants and their attributes.
func TestParseMasterPlaylist(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=1280000,AVERAGE-BANDWIDTH=1000000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"
low/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000,RESOLUTION=1280x720
high/index.m3u8
`
	p := mustParse(t, raw)

	if !p.IsMaster {
		t.Fatalf("IsMaster = false, want true")
	}
	if len(p.Segments) != 0 {
		t.Errorf("len(Segments) = %d, want 0", len(p.Segments))
	}
	if len(p.Variants) != 2 {
		t.Fatalf("len(Variants) = %d, want 2", len(p.Variants))
	}

	want := []Variant{
		{URI: "low/index.m3u8", Bandwidth: 1280000, AverageBandwidth: 1000000, Width: 640, Height: 360, Codecs: "avc1.4d401e,mp4a.40.2"},
		{URI: "high/index.m3u8", Bandwidth: 2560000, AverageBandwidth: 0, Width: 1280, Height: 720, Codecs: ""},
	}
	for i, w := range want {
		if got := p.Variants[i]; got != w {
			t.Errorf("variants[%d] = %+v, want %+v", i, got, w)
		}
	}
}

// Test 5: playlists that do not start with #EXTM3U are rejected.
func TestParseMissingHeader(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":       "",
		"first line":  "#EXT-X-VERSION:3\n#EXTINF:1,\na.ts\n",
		"media only":  "#EXTINF:1,\na.ts\n",
		"blank first": "\n#EXTM3U\n",
	} {
		if _, err := Parse(strings.NewReader(raw)); err == nil {
			t.Errorf("%s: Parse() error = nil, want an error", name)
		} else if !errors.Is(err, ErrMissingHeader) {
			t.Errorf("%s: Parse() error = %v, want ErrMissingHeader", name, err)
		}
	}
}

// Test 6: a media playlist without segments is an error.
func TestParseNoSegments(t *testing.T) {
	for name, raw := range map[string]string{
		"header only": "#EXTM3U\n",
		"tags only":   "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-ENDLIST\n",
	} {
		_, err := Parse(strings.NewReader(raw))
		if !errors.Is(err, ErrNoSegments) {
			t.Errorf("%s: Parse() error = %v, want ErrNoSegments", name, err)
		}
	}
}

// Test that unknown tags are ignored while parsing but kept in Raw.
func TestParseIgnoresUnknownTags(t *testing.T) {
	const raw = `#EXTM3U
#EXT-X-VERSION:3
# a comment
#EXT-X-ALLOW-CACHE:NO
#EXT-X-PROGRAM-DATE-TIME:2024-05-01T10:00:00Z
#EXTINF:5.5,
a.ts
#EXT-X-DISCONTINUITY
#EXTINF:6.5,
b.ts
#EXT-X-ENDLIST
`
	p := mustParse(t, raw)
	if len(p.Segments) != 2 {
		t.Fatalf("len(Segments) = %d, want 2", len(p.Segments))
	}
	if !almostEqual(p.Segments[0].Duration, 5.5) || !almostEqual(p.Segments[1].Duration, 6.5) {
		t.Errorf("durations = %v/%v, want 5.5/6.5", p.Segments[0].Duration, p.Segments[1].Duration)
	}
	if !strings.Contains(p.Raw, "#EXT-X-DISCONTINUITY") {
		t.Errorf("Raw lost unknown tags")
	}
}

// Test that a nil reader and a failing reader are reported.
func TestParseReaderErrors(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Errorf("Parse(nil) error = nil, want an error")
	}
	if _, err := Parse(errReader{}); err == nil {
		t.Errorf("Parse(failing reader) error = nil, want an error")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// Test the small parsing helpers that other packages rely on indirectly.
func TestParseAttrs(t *testing.T) {
	attrs := parseAttrs(`METHOD=AES-128,URI="https://example.com/a,b.key",RESOLUTION=1920x1080`)
	if attrs["METHOD"] != "AES-128" {
		t.Errorf("METHOD = %q", attrs["METHOD"])
	}
	if attrs["URI"] != "https://example.com/a,b.key" {
		t.Errorf("URI = %q, want the comma inside the quotes", attrs["URI"])
	}
	if attrs["RESOLUTION"] != "1920x1080" {
		t.Errorf("RESOLUTION = %q", attrs["RESOLUTION"])
	}
}
