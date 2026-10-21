// Package hls implements a small, dependency-free parser and rewriter for
// HTTP Live Streaming (HLS) playlists as described by RFC 8216.
//
// The parser understands the subset of the specification that is needed to
// proxy and cache a stream locally: master playlists with their variants, media
// playlists with segments, encryption keys, initialization sections and byte
// ranges. Unknown tags and comments are ignored by Parse but are preserved
// verbatim in Playlist.Raw, which Rewrite uses to keep the original document
// intact apart from the URIs it replaces.
package hls

import (
	"errors"
	"io"
	"strconv"
	"strings"
)

// Sentinel errors returned by Parse and Rewrite.
var (
	// ErrMissingHeader is returned when a playlist does not start with #EXTM3U.
	ErrMissingHeader = errors.New("hls: playlist does not start with #EXTM3U")
	// ErrNoSegments is returned for a media playlist without any segment.
	ErrNoSegments = errors.New("hls: media playlist has no segments")
	// ErrMasterPlaylist is returned when Rewrite is called on a master playlist.
	ErrMasterPlaylist = errors.New("hls: rewrite of a master playlist is not supported")
)

// ByteRange is a sub-range of a resource, expressed as a length and the offset
// of its first byte.
type ByteRange struct {
	Length int64
	Offset int64
}

// Key is the content of an #EXT-X-KEY tag. An empty Method means the section
// starting at the segment is not encrypted (the tag was METHOD=NONE).
type Key struct {
	Method            string
	URI               string
	IV                string
	KeyFormat         string
	KeyFormatVersions string
}

// Map is the content of an #EXT-X-MAP tag: the initialization section that
// precedes the media segments it applies to.
type Map struct {
	URI       string
	ByteRange *ByteRange
}

// Segment is a single media segment of a media playlist.
type Segment struct {
	URI       string
	Duration  float64
	Title     string
	Sequence  int
	ByteRange *ByteRange
	Key       *Key
	Map       *Map
}

// Variant is a single #EXT-X-STREAM-INF entry of a master playlist.
type Variant struct {
	URI              string
	Bandwidth        int
	AverageBandwidth int
	Width, Height    int
	Codecs           string
}

// Playlist is a parsed master or media playlist.
type Playlist struct {
	Version        int
	TargetDuration int
	MediaSequence  int
	IsMaster       bool
	Variants       []Variant
	Segments       []Segment
	Raw            string
}

// Parse reads an HLS playlist. A media playlist must contain at least one
// segment, otherwise ErrNoSegments is returned.
func Parse(r io.Reader) (*Playlist, error) {
	if r == nil {
		return nil, errors.New("hls: nil reader")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return parseString(string(data))
}

// parseString is the string form of Parse; it keeps the original document in
// Playlist.Raw.
func parseString(raw string) (*Playlist, error) {
	p := &Playlist{Raw: raw}

	lines := splitLines(raw)
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, ErrMissingHeader
	}

	var (
		// Pending state collected from the tags seen so far.
		pendingDuration float64
		pendingTitle    string
		pendingKey      *Key
		pendingMap      *Map
		pendingRange    *ByteRange
		pendingVariant  *Variant

		// lastRangeEnd is the first byte after the previous byte range and is
		// used when a #EXT-X-BYTERANGE tag omits its offset.
		lastRangeEnd int64
	)

	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			switch {
			case strings.HasPrefix(trimmed, tagStreamInf):
				p.IsMaster = true
				v := variantFromAttrs(parseAttrs(trimmed[len(tagStreamInf):]))
				pendingVariant = &v

			case strings.HasPrefix(trimmed, tagIFrameStreamInf):
				// I-frame only renditions do not carry a separate media
				// playlist variant, but their presence marks a master playlist.
				p.IsMaster = true

			case strings.HasPrefix(trimmed, tagMedia):
				p.IsMaster = true

			case strings.HasPrefix(trimmed, tagInf):
				pendingDuration, pendingTitle = parseInf(trimmed[len(tagInf):])

			case strings.HasPrefix(trimmed, tagKey):
				pendingKey = keyFromAttrs(parseAttrs(trimmed[len(tagKey):]))

			case strings.HasPrefix(trimmed, tagMap):
				pendingMap = mapFromAttrs(parseAttrs(trimmed[len(tagMap):]))

			case strings.HasPrefix(trimmed, tagByteRange):
				pendingRange = parseByteRange(trimmed[len(tagByteRange):], lastRangeEnd)

			case strings.HasPrefix(trimmed, tagVersion):
				p.Version = parseInt(trimmed[len(tagVersion):])

			case strings.HasPrefix(trimmed, tagTargetDuration):
				p.TargetDuration = parseInt(trimmed[len(tagTargetDuration):])

			case strings.HasPrefix(trimmed, tagMediaSequence):
				p.MediaSequence = parseInt(trimmed[len(tagMediaSequence):])

			default:
				// Comments and tags without meaning for us are ignored.
			}
			continue
		}

		// A URI line. In a master playlist it belongs to the preceding
		// #EXT-X-STREAM-INF tag, otherwise it is a media segment.
		if pendingVariant != nil {
			pendingVariant.URI = trimmed
			p.Variants = append(p.Variants, *pendingVariant)
			pendingVariant = nil
			continue
		}

		seg := Segment{
			URI:       trimmed,
			Duration:  pendingDuration,
			Title:     pendingTitle,
			ByteRange: pendingRange,
		}
		if pendingKey != nil {
			k := *pendingKey
			seg.Key = &k
		}
		if pendingMap != nil {
			m := *pendingMap
			if m.ByteRange != nil {
				br := *m.ByteRange
				m.ByteRange = &br
			}
			seg.Map = &m
		}
		if pendingRange != nil {
			lastRangeEnd = pendingRange.Offset + pendingRange.Length
		}
		pendingRange = nil
		pendingDuration = 0
		pendingTitle = ""

		p.Segments = append(p.Segments, seg)
	}

	if !p.IsMaster && len(p.Segments) == 0 {
		return nil, ErrNoSegments
	}

	// Number the segments once #EXT-X-MEDIA-SEQUENCE is known, wherever in the
	// document it appeared.
	for i := range p.Segments {
		p.Segments[i].Sequence = p.MediaSequence + i
	}

	return p, nil
}

// Tag prefixes handled by the parser.
const (
	tagStreamInf       = "#EXT-X-STREAM-INF:"
	tagIFrameStreamInf = "#EXT-X-I-FRAME-STREAM-INF:"
	tagMedia           = "#EXT-X-MEDIA:"
	tagInf             = "#EXTINF:"
	tagKey             = "#EXT-X-KEY:"
	tagMap             = "#EXT-X-MAP:"
	tagByteRange       = "#EXT-X-BYTERANGE:"
	tagVersion         = "#EXT-X-VERSION:"
	tagTargetDuration  = "#EXT-X-TARGETDURATION:"
	tagMediaSequence   = "#EXT-X-MEDIA-SEQUENCE:"
)

// variantFromAttrs builds a Variant from the attributes of #EXT-X-STREAM-INF.
func variantFromAttrs(attrs map[string]string) Variant {
	v := Variant{
		Bandwidth:        parseInt(attrs["BANDWIDTH"]),
		AverageBandwidth: parseInt(attrs["AVERAGE-BANDWIDTH"]),
		Codecs:           attrs["CODECS"],
	}
	if res, ok := attrs["RESOLUTION"]; ok {
		v.Width, v.Height = parseResolution(res)
	}
	return v
}

// keyFromAttrs builds a Key from the attributes of #EXT-X-KEY. It returns nil
// for METHOD=NONE, which means the following segments are not encrypted.
func keyFromAttrs(attrs map[string]string) *Key {
	k := &Key{
		Method:            attrs["METHOD"],
		URI:               attrs["URI"],
		IV:                attrs["IV"],
		KeyFormat:         attrs["KEYFORMAT"],
		KeyFormatVersions: attrs["KEYFORMATVERSIONS"],
	}
	if strings.EqualFold(k.Method, "NONE") {
		return nil
	}
	return k
}

// mapFromAttrs builds a Map from the attributes of #EXT-X-MAP. It returns nil
// when the tag has no URI.
func mapFromAttrs(attrs map[string]string) *Map {
	uri := attrs["URI"]
	if uri == "" {
		return nil
	}
	m := &Map{URI: uri}
	if br, ok := attrs["BYTERANGE"]; ok && strings.TrimSpace(br) != "" {
		m.ByteRange = parseByteRange(br, 0)
	}
	return m
}

// parseInf splits the payload of an #EXTINF tag into duration and title.
func parseInf(payload string) (float64, string) {
	duration, title := payload, ""
	if i := strings.Index(payload, ","); i >= 0 {
		duration, title = payload[:i], payload[i+1:]
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(duration), 64)
	if err != nil {
		return 0, strings.TrimSpace(title)
	}
	return d, strings.TrimSpace(title)
}

// parseByteRange parses "<length>[@<offset>]". When the offset is omitted the
// range starts at fallbackOffset, the end of the previous byte range.
func parseByteRange(payload string, fallbackOffset int64) *ByteRange {
	payload = strings.TrimSpace(payload)
	lengthPart, offsetPart := payload, ""
	haveOffset := false
	if i := strings.Index(payload, "@"); i >= 0 {
		lengthPart, offsetPart = payload[:i], payload[i+1:]
		haveOffset = true
	}

	length, err := strconv.ParseInt(strings.TrimSpace(lengthPart), 10, 64)
	if err != nil {
		return nil
	}

	br := &ByteRange{Length: length, Offset: fallbackOffset}
	if haveOffset {
		offset, err := strconv.ParseInt(strings.TrimSpace(offsetPart), 10, 64)
		if err != nil {
			return nil
		}
		br.Offset = offset
	}
	return br
}

// parseResolution parses a "<width>x<height>" resolution attribute.
func parseResolution(s string) (int, int) {
	i := strings.IndexAny(s, "xX")
	if i < 0 {
		return 0, 0
	}
	return parseInt(s[:i]), parseInt(s[i+1:])
}

// parseInt parses a decimal integer, returning 0 when the value is absent or
// malformed.
func parseInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	// Tolerate values that some encoders emit as floats, e.g. "10.0".
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

// splitLines normalises line endings and removes a leading byte order mark.
func splitLines(raw string) []string {
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	return strings.Split(raw, "\n")
}

// parseAttrs parses an HLS attribute list ("NAME=value,NAME=\"value,...\"")
// into a map keyed by the upper-cased attribute name. Quoted values may contain
// commas and equal signs.
func parseAttrs(s string) map[string]string {
	attrs := make(map[string]string)
	eachAttr(s, func(name, value string, _, _ int) {
		if name == "" {
			return
		}
		attrs[strings.ToUpper(name)] = value
	})
	return attrs
}

// eachAttr walks an HLS attribute list and reports every attribute together
// with the span of its raw value inside s. For quoted values the span excludes
// the surrounding quotes, so callers can splice in a replacement value while
// keeping the quoting intact.
func eachAttr(s string, fn func(name, value string, from, to int)) {
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ',' || s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return
		}

		nameStart := i
		for i < len(s) && s[i] != '=' && s[i] != ',' {
			i++
		}
		name := strings.TrimSpace(s[nameStart:i])
		if i >= len(s) && name == "" {
			return
		}
		if i >= len(s) || s[i] != '=' {
			// Attribute without a value; consume the separator and carry on.
			if i < len(s) {
				i++
			}
			continue
		}

		i++ // '='
		var value string
		var from, to int
		if i < len(s) && s[i] == '"' {
			i++
			from = i
			for i < len(s) && s[i] != '"' {
				i++
			}
			to = i
			value = s[from:to]
			if i < len(s) {
				i++ // closing quote
			}
		} else {
			from = i
			for i < len(s) && s[i] != ',' {
				i++
			}
			value = strings.TrimSpace(s[from:i])
			to = from + len(value)
		}

		fn(name, value, from, to)
	}
}
