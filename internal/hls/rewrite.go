package hls

import (
	"fmt"
	"strconv"
	"strings"
)

// Kinds of assets reported by Rewrite.
const (
	KindSegment = "segment"
	KindKey     = "key"
	KindMap     = "map"
)

// AssetRef links a URI found in the original playlist to the local file name
// that Rewrite substituted for it. The caller downloads SourceURI and stores it
// under LocalName.
type AssetRef struct {
	Kind      string
	SourceURI string
	LocalName string
}

// RewriteOptions tunes Rewrite. It is currently empty and reserved for future
// use such as a custom local naming scheme.
type RewriteOptions struct{}

// Rewrite turns a media playlist into one whose URIs point at local files:
// segments become "seg-00001.ts" (extension taken from the source URI, ".ts"
// when it has none), keys become "key-01.key" and initialization sections
// become "init-01.mp4" (extension taken from the source, ".mp4" when absent).
//
// Every other line of the playlist is kept verbatim, including tags the parser
// does not model, so the result round-trips through Parse with the same segment
// count and durations. Variant declarations (#EXT-X-STREAM-INF) are dropped
// because a media playlist must not contain them.
//
// Assets are returned in order of first appearance in the playlist.
func Rewrite(p *Playlist, opts RewriteOptions) (out []byte, assets []AssetRef, err error) {
	if p == nil {
		return nil, nil, errNilPlaylist
	}
	if p.IsMaster {
		return nil, nil, ErrMasterPlaylist
	}
	if len(p.Segments) == 0 {
		return nil, nil, ErrNoSegments
	}

	r := newRewriter()
	if lines := splitLines(p.Raw); len(lines) > 0 && strings.TrimSpace(lines[0]) == "#EXTM3U" {
		r.rewriteLines(lines)
	}
	// Fall back to rebuilding the document from the parsed structure when the
	// original text did not carry exactly one URI per segment.
	if r.segN != len(p.Segments) {
		r = newRewriter()
		r.serialize(p)
	}

	return []byte(r.String()), r.assets, nil
}

// BuildMaster renders a master playlist for the given variants.
func BuildMaster(variants []Variant) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")

	for _, v := range variants {
		var attrs []string
		if v.Bandwidth > 0 {
			attrs = append(attrs, "BANDWIDTH="+strconv.Itoa(v.Bandwidth))
		}
		if v.AverageBandwidth > 0 {
			attrs = append(attrs, "AVERAGE-BANDWIDTH="+strconv.Itoa(v.AverageBandwidth))
		}
		if v.Width > 0 && v.Height > 0 {
			attrs = append(attrs, "RESOLUTION="+strconv.Itoa(v.Width)+"x"+strconv.Itoa(v.Height))
		}
		if v.Codecs != "" {
			attrs = append(attrs, `CODECS="`+v.Codecs+`"`)
		}

		b.WriteString("\n#EXT-X-STREAM-INF:")
		b.WriteString(strings.Join(attrs, ","))
		b.WriteString("\n")
		b.WriteString(v.URI)
		b.WriteString("\n")
	}

	return []byte(b.String())
}

// errNilPlaylist is returned when Rewrite is handed a nil playlist.
var errNilPlaylist = fmt.Errorf("hls: nil playlist")

// rewriter replaces URIs while streaming through a playlist document.
type rewriter struct {
	lines    []string
	assets   []AssetRef
	keyNames map[string]string
	mapNames map[string]string
	segN     int
	keyN     int
	mapN     int
}

func newRewriter() *rewriter {
	return &rewriter{
		keyNames: make(map[string]string),
		mapNames: make(map[string]string),
	}
}

// String renders the rewriter's lines as a playlist document terminated by a
// single newline.
func (r *rewriter) String() string {
	s := strings.Join(r.lines, "\n")
	s = strings.TrimRight(s, "\n")
	return s + "\n"
}

func (r *rewriter) append(line string) { r.lines = append(r.lines, line) }

// rewriteLines walks the original document, keeping every line and replacing
// only the URIs of segments, keys and initialization sections.
func (r *rewriter) rewriteLines(in []string) {
	skipNextURI := false

	for _, line := range in {
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			r.append("")

		case strings.HasPrefix(trimmed, tagStreamInf),
			strings.HasPrefix(trimmed, tagIFrameStreamInf):
			// A media playlist carries no variants; drop the declaration and
			// the URI line that belongs to it.
			skipNextURI = true

		case strings.HasPrefix(trimmed, tagKey):
			r.append(r.rewriteTagURI(trimmed, tagKey, false))

		case strings.HasPrefix(trimmed, tagMap):
			r.append(r.rewriteTagURI(trimmed, tagMap, true))

		case strings.HasPrefix(trimmed, "#"):
			r.append(trimmed)

		default:
			if skipNextURI {
				skipNextURI = false
				continue
			}
			r.append(r.localSegmentName(trimmed))
		}
	}
}

// rewriteTagURI replaces the URI attribute of a tag. withExt selects whether
// the local name keeps the extension of the source URI.
func (r *rewriter) rewriteTagURI(tag, prefix string, withExt bool) string {
	attrs := tag[len(prefix):]
	uri, from, to, ok := findAttr(attrs, "URI")
	if !ok || uri == "" {
		return tag
	}

	// Key yang tertanam sebagai data URI tidak perlu diunduh dan harus
	// dipertahankan apa adanya agar player bisa mendekripsi segmen.
	if isDataURI(uri) {
		return tag
	}

	var local string
	if withExt {
		local = r.localMapName(uri)
	} else {
		local = r.localKeyName(uri)
	}

	return prefix + attrs[:from] + local + attrs[to:]
}

// isDataURI melaporkan apakah URI berupa data URI (mis. key AES-128 yang
// tertanam di playlist).
func isDataURI(uri string) bool {
	return len(uri) >= 5 && strings.EqualFold(uri[:5], "data:")
}

// localSegmentName registers a segment and returns its local file name.
func (r *rewriter) localSegmentName(src string) string {
	r.segN++
	name := fmt.Sprintf("seg-%05d.%s", r.segN, extOf(src, "ts"))
	r.assets = append(r.assets, AssetRef{Kind: KindSegment, SourceURI: src, LocalName: name})
	return name
}

// localKeyName registers a key and returns its local file name. A key used by
// several segments keeps a single name.
func (r *rewriter) localKeyName(src string) string {
	if name, ok := r.keyNames[src]; ok {
		return name
	}
	r.keyN++
	name := fmt.Sprintf("key-%02d.key", r.keyN)
	r.keyNames[src] = name
	r.assets = append(r.assets, AssetRef{Kind: KindKey, SourceURI: src, LocalName: name})
	return name
}

// localMapName registers an initialization section and returns its local file
// name.
func (r *rewriter) localMapName(src string) string {
	if name, ok := r.mapNames[src]; ok {
		return name
	}
	r.mapN++
	name := fmt.Sprintf("init-%02d.%s", r.mapN, extOf(src, "mp4"))
	r.mapNames[src] = name
	r.assets = append(r.assets, AssetRef{Kind: KindMap, SourceURI: src, LocalName: name})
	return name
}

// serialize rebuilds a media playlist from the parsed structure. It is the
// fallback for playlists whose Raw document is unavailable or does not match
// the parsed segments.
func (r *rewriter) serialize(p *Playlist) {
	r.append("#EXTM3U")
	if p.Version > 0 {
		r.append("#EXT-X-VERSION:" + strconv.Itoa(p.Version))
	}
	if p.TargetDuration > 0 {
		r.append("#EXT-X-TARGETDURATION:" + strconv.Itoa(p.TargetDuration))
	}
	r.append("#EXT-X-MEDIA-SEQUENCE:" + strconv.Itoa(p.MediaSequence))

	var prevKey *Key
	var prevMap *Map
	for _, seg := range p.Segments {
		if !sameKey(prevKey, seg.Key) {
			if seg.Key != nil {
				r.append(r.rewriteTagURI(renderKeyTag(seg.Key), tagKey, false))
			} else if prevKey != nil {
				r.append("#EXT-X-KEY:METHOD=NONE")
			}
			prevKey = seg.Key
		}
		if !sameMap(prevMap, seg.Map) && seg.Map != nil {
			r.append(r.rewriteTagURI(renderMapTag(seg.Map), tagMap, true))
			prevMap = seg.Map
		}
		if seg.ByteRange != nil {
			r.append("#EXT-X-BYTERANGE:" + strconv.FormatInt(seg.ByteRange.Length, 10) +
				"@" + strconv.FormatInt(seg.ByteRange.Offset, 10))
		}
		r.append("#EXTINF:" + strconv.FormatFloat(seg.Duration, 'f', -1, 64) + "," + seg.Title)
		r.append(r.localSegmentName(seg.URI))
	}
	r.append("#EXT-X-ENDLIST")
}

// renderKeyTag renders an #EXT-X-KEY tag for k.
func renderKeyTag(k *Key) string {
	attrs := []string{"METHOD=" + k.Method}
	if k.URI != "" {
		attrs = append(attrs, `URI="`+k.URI+`"`)
	}
	if k.IV != "" {
		attrs = append(attrs, "IV="+k.IV)
	}
	if k.KeyFormat != "" {
		attrs = append(attrs, `KEYFORMAT="`+k.KeyFormat+`"`)
	}
	if k.KeyFormatVersions != "" {
		attrs = append(attrs, `KEYFORMATVERSIONS="`+k.KeyFormatVersions+`"`)
	}
	return tagKey + strings.Join(attrs, ",")
}

// renderMapTag renders an #EXT-X-MAP tag for m.
func renderMapTag(m *Map) string {
	attrs := `URI="` + m.URI + `"`
	if m.ByteRange != nil {
		attrs += `,BYTERANGE="` + strconv.FormatInt(m.ByteRange.Length, 10) +
			"@" + strconv.FormatInt(m.ByteRange.Offset, 10) + `"`
	}
	return tagMap + attrs
}

func sameKey(a, b *Key) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameMap(a, b *Map) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.URI != b.URI {
		return false
	}
	if a.ByteRange == nil || b.ByteRange == nil {
		return a.ByteRange == nil && b.ByteRange == nil
	}
	return *a.ByteRange == *b.ByteRange
}

// findAttr locates the named attribute in an attribute list and reports the
// span of its raw value, excluding any surrounding quotes.
func findAttr(attrs, want string) (value string, from, to int, ok bool) {
	eachAttr(attrs, func(name, v string, f, t int) {
		if ok || !strings.EqualFold(name, want) {
			return
		}
		value, from, to, ok = v, f, t, true
	})
	return value, from, to, ok
}

// extOf returns the file extension of a URI, or def when it has none. Query
// strings and fragments are ignored.
func extOf(uri, def string) string {
	u := uri
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	if i := strings.LastIndexAny(u, "/\\"); i >= 0 {
		u = u[i+1:]
	}
	i := strings.LastIndex(u, ".")
	if i < 0 || i == len(u)-1 {
		return def
	}
	ext := strings.ToLower(u[i+1:])
	if !isAlnum(ext) {
		return def
	}
	return ext
}

// isAlnum reports whether s is non-empty and made of ASCII letters and digits.
func isAlnum(s string) bool {
	if s == "" || len(s) > 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}
