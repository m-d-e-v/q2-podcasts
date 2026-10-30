// Package feed reads podcast feeds: RSS 2.0 with Apple's itunes: tags, which nearly every podcast
// publishes, and Atom with enclosure links. It is lenient where real feeds are sloppy: HTML entities,
// Latin-1 and Windows-1252, stray control characters, a dozen date forms and misdeclared
// namespaces. Only what the Q2 needs is kept: no show notes, which can run to megabytes.
package feed

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"q2-podcasts/internal/q2spot"
)

// MaxText bounds a kept field; anything longer is cut on a character boundary.
const MaxText = 1024

type Feed struct {
	Title, Author, Image string // Title "" when the feed gives none
	NewURL               string // itunes:new-feed-url: the feed has moved there
	Episodes             []Episode
}

type Episode struct {
	GUID     string    `json:"guid"`
	Title    string    `json:"title"`
	Date     time.Time `json:"date"`
	Duration int       `json:"duration,omitempty"` // seconds; 0 unknown
	URL      string    `json:"url"`
	Type     string    `json:"type,omitempty"`
	Length   int64     `json:"length,omitempty"` // bytes, as the feed claims; often wrong
	Image    string    `json:"image,omitempty"`  // its own art (itunes:image), when it has one
}

var ErrNotFeed = errors.New("feed: not an RSS or Atom feed")

// Parse reads a feed. A feed cut short (a dropped connection, a size limit) keeps the episodes read
// whole before the break. Episodes are newest first; ones without an audio enclosure are left out.
func Parse(r io.Reader) (*Feed, error) {
	d := xml.NewDecoder(clean(r))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charsetReader
	f := &Feed{}
	var stack []string
	root := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if len(f.Episodes) > 0 {
				break
			}
			if !root {
				return nil, ErrNotFeed
			}
			return nil, fmt.Errorf("feed: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if !root {
				if name != "rss" && name != "feed" && name != "RDF" {
					return nil, ErrNotFeed
				}
				root = true
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			if name == "item" || (name == "entry" && parent == "feed") {
				ep, err := item(d, name == "entry")
				if err != nil { // cut off inside: its guid or title may be missing or short
					if len(f.Episodes) == 0 {
						return nil, fmt.Errorf("feed: %w", err)
					}
					return finish(f), nil
				}
				if ep.URL != "" {
					f.Episodes = append(f.Episodes, ep)
				}
				continue
			}
			if parent == "channel" || parent == "feed" {
				if channel(d, t, f) {
					continue
				}
			}
			if parent == "image" && name == "url" && f.Image == "" {
				f.Image = text(d)
				continue
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if !root {
		return nil, ErrNotFeed
	}
	return finish(f), nil
}

func finish(f *Feed) *Feed {
	f.Episodes = identify(f.Episodes)
	sort.SliceStable(f.Episodes, func(i, j int) bool { return f.Episodes[i].Date.After(f.Episodes[j].Date) })
	return f
}

// identify gives each episode an id of its own. Episodes sharing a guid (a feed that gives every
// item its site's address, or links told apart only by their query where there is no guid) are told
// apart by their link: its path where that differs, else all of it. One listed twice with the same
// guid and link is kept once.
func identify(eps []Episode) []Episode {
	type both struct{ id, url string }
	seen := map[both]bool{}
	by := map[string][]int{}
	var out []Episode
	for _, e := range eps {
		if k := (both{e.GUID, e.URL}); !seen[k] {
			seen[k] = true
			by[e.GUID] = append(by[e.GUID], len(out))
			out = append(out, e)
		}
	}
	for id, is := range by {
		if len(is) == 1 {
			continue
		}
		paths := map[string]bool{}
		for _, i := range is {
			paths[urlPath(out[i].URL)] = true
		}
		for _, i := range is {
			if len(paths) == len(is) {
				out[i].GUID = id + " " + urlPath(out[i].URL)
			} else {
				out[i].GUID = id + " " + out[i].URL
			}
		}
	}
	return out
}

func itunes(n xml.Name) bool {
	return strings.Contains(strings.ToLower(n.Space), "itunes")
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

// channel takes a child of <channel> (or Atom's <feed>) that it knows, consuming it; false leaves it
// for the caller.
func channel(d *xml.Decoder, t xml.StartElement, f *Feed) bool {
	switch t.Name.Local {
	case "title":
		if s := text(d); f.Title == "" || !itunes(t.Name) {
			if s != "" {
				f.Title = s
			}
		}
	case "author":
		if s := text(d); s != "" && (f.Author == "" || itunes(t.Name)) {
			f.Author = s
		}
	case "image":
		if !itunes(t.Name) {
			return false // RSS's <image><url>, read as the parser walks it
		}
		if h := attr(t, "href"); h != "" {
			f.Image = h
		}
		d.Skip()
	case "new-feed-url":
		f.NewURL = text(d)
	default:
		return false
	}
	return true
}

// item reads one <item> or <entry> up to its end. An error keeps what was read.
func item(d *xml.Decoder, atom bool) (Episode, error) {
	var ep Episode
	var alt, guid, encType string
	var media Episode // media:content, when there is no enclosure
	for {
		tok, err := d.Token()
		if err != nil {
			return done(ep, alt, guid, media, encType), err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return done(ep, alt, guid, media, encType), nil
		case xml.StartElement:
			switch name := t.Name.Local; {
			case name == "title":
				if s := text(d); itunes(t.Name) {
					alt = s
				} else if ep.Title == "" {
					ep.Title = s
				}
			case name == "guid" || (name == "id" && atom):
				guid = text(d)
			case name == "pubDate" || name == "published" || name == "date" || name == "updated" || name == "issued":
				if s := text(d); ep.Date.IsZero() || name == "pubDate" || name == "published" {
					if when := Date(s); !when.IsZero() {
						ep.Date = when
					}
				}
			case name == "duration" && itunes(t.Name):
				ep.Duration = Duration(text(d))
			case name == "image" && itunes(t.Name):
				ep.Image = attr(t, "href")
				d.Skip()
			case name == "enclosure" && ep.URL == "":
				if u := attr(t, "url"); u != "" && Audio(attr(t, "type"), u) {
					ep.URL, encType = u, attr(t, "type")
					ep.Length, _ = strconv.ParseInt(attr(t, "length"), 10, 64)
				}
				d.Skip()
			case name == "link" && atom && attr(t, "rel") == "enclosure" && ep.URL == "":
				if u := attr(t, "href"); u != "" && Audio(attr(t, "type"), u) {
					ep.URL, encType = u, attr(t, "type")
					ep.Length, _ = strconv.ParseInt(attr(t, "length"), 10, 64)
				}
				d.Skip()
			case name == "content" && strings.Contains(t.Name.Space, "media") && media.URL == "":
				if u := attr(t, "url"); u != "" && (attr(t, "medium") == "audio" || Audio(attr(t, "type"), u)) {
					media.URL, media.Type = u, attr(t, "type")
					media.Length, _ = strconv.ParseInt(attr(t, "fileSize"), 10, 64)
				}
				d.Skip()
			default:
				d.Skip()
			}
		}
	}
}

func done(ep Episode, alt, guid string, media Episode, encType string) Episode {
	if ep.Title == "" {
		ep.Title = alt
	}
	if ep.Title == "" {
		ep.Title = "Untitled episode"
	}
	if ep.URL == "" {
		ep.URL, encType, ep.Length = media.URL, media.Type, media.Length
	}
	ep.Type = encType
	if ep.Length < 0 {
		ep.Length = 0
	}
	ep.GUID = guid
	if ep.GUID == "" { // the link without its query, which a paid feed's signed links change each fetch
		ep.GUID = urlPath(ep.URL)
	}
	if ep.GUID == "" {
		ep.GUID = ep.Title + "|" + ep.Date.UTC().Format(time.RFC3339)
	}
	return ep
}

// text is an element's character data, nested elements' included, up to its end, whitespace
// collapsed and cut to MaxText.
func text(d *xml.Decoder) string {
	var b strings.Builder
	for depth := 1; depth > 0; {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if b.Len() < 4*MaxText {
				b.Write(t)
			}
		}
	}
	return q2spot.Cut(strings.Join(strings.Fields(b.String()), " "), MaxText)
}

// audioExt are the file kinds the Q2's player lists and plays.
var audioExt = map[string]bool{".mp3": true, ".m4a": true, ".m4b": true, ".aac": true, ".ogg": true,
	".oga": true, ".opus": true, ".flac": true, ".wav": true, ".mp4": true}

// Audio reports whether an enclosure of this MIME type at this URL is sound the Q2 can play: an
// audio type, or no useful type and an audio file name. Video is left out.
func Audio(typ, url string) bool {
	typ = strings.ToLower(strings.TrimSpace(typ))
	if i := strings.IndexByte(typ, ';'); i >= 0 {
		typ = strings.TrimSpace(typ[:i])
	}
	switch {
	case strings.HasPrefix(typ, "audio/"):
		return true
	case strings.HasPrefix(typ, "video/"):
		return false
	}
	ext := strings.ToLower(path.Ext(urlPath(url)))
	return audioExt[ext] && ext != ".mp4" || (ext == ".mp4" && typ == "")
}

// Ext is the extension to save an enclosure under: its URL's when that is audio, else its type's;
// MP4 audio as .m4a, which the Q2's file lists take.
func Ext(typ, url string) string {
	ext := strings.ToLower(path.Ext(urlPath(url)))
	if ext == ".mp4" {
		return ".m4a"
	}
	if audioExt[ext] {
		return ext
	}
	typ = strings.ToLower(typ)
	switch {
	case strings.Contains(typ, "mpeg"), strings.Contains(typ, "mp3"):
		return ".mp3"
	case strings.Contains(typ, "mp4"), strings.Contains(typ, "m4a"):
		return ".m4a"
	case strings.Contains(typ, "aac"):
		return ".aac"
	case strings.Contains(typ, "opus"):
		return ".opus"
	case strings.Contains(typ, "ogg"):
		return ".ogg"
	case strings.Contains(typ, "flac"):
		return ".flac"
	case strings.Contains(typ, "wav"):
		return ".wav"
	}
	return ".mp3"
}

func urlPath(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

var zones = map[string]string{"UT": "+0000", "UTC": "+0000", "GMT": "+0000", "Z": "+0000",
	"EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500", "MST": "-0700", "MDT": "-0600",
	"PST": "-0800", "PDT": "-0700", "BST": "+0100", "CET": "+0100", "CEST": "+0200", "AEST": "+1000",
	"AEDT": "+1100"}

var layouts = []string{
	"2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04 -0700", "2 Jan 2006 15:04:05", "2 Jan 2006",
	"2 Jan 2006 15:04:05 -07:00",
	"2 January 2006 15:04:05 -0700", "2 January 2006 15:04 -0700", "2 January 2006",
	"Jan 2 2006 15:04:05 -0700", "January 2 2006", "Jan 2 2006",
	time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02T15:04Z07:00",
	"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05", "2006-01-02",
}

// Date reads a feed's date: RFC 822 as RSS has it (and as feeds really write it: no weekday, a full
// weekday or month, one-digit days, zone names, no seconds) or ISO 8601 as Atom has it. Zero when
// none fits.
func Date(s string) time.Time {
	f := strings.Fields(strings.NewReplacer(",", " ").Replace(s))
	if len(f) == 0 {
		return time.Time{}
	}
	if c := f[0][0]; (c|32) >= 'a' && (c|32) <= 'z' && len(f) > 3 {
		if _, err := time.Parse("Jan", f[0][:min(3, len(f[0]))]); err != nil {
			f = f[1:] // a weekday, not a month
		}
	}
	if z, ok := zones[strings.ToUpper(f[len(f)-1])]; ok {
		f[len(f)-1] = z
	}
	s = strings.Join(f, " ")
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	// A full month name in a short layout's place, as "Sept" or "June".
	if len(f) >= 3 && len(f[1]) > 3 {
		f[1] = f[1][:3]
		s = strings.Join(f, " ")
		for _, l := range layouts[:4] {
			if t, err := time.Parse(l, s); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// Duration reads itunes:duration: seconds ("3723", "3723.5"), or "MM:SS" or "H:MM:SS". 0 when it
// is none of those.
func Duration(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	total := 0
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0
	}
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// clean drops a UTF-8 byte order mark and leading whitespace, turns the control characters XML
// forbids into spaces and, in a feed said to be UTF-8, reads a byte that isn't as Windows-1252;
// feeds made by hand carry all four.
func clean(r io.Reader) io.Reader {
	br := bufio.NewReaderSize(r, 64<<10)
	if b, _ := br.Peek(3); len(b) == 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf {
		br.Discard(3)
	}
	for {
		b, err := br.Peek(1)
		if err != nil || (b[0] != ' ' && b[0] != '\t' && b[0] != '\r' && b[0] != '\n') {
			break
		}
		br.Discard(1)
	}
	head, _ := br.Peek(256)
	if m := declared.FindSubmatch(head); m == nil || utf8Name[strings.ToLower(string(m[1]))] {
		return &controls{r: &mended{r: br}}
	}
	return &controls{r: br} // another character set, read by charsetReader
}

var declared = regexp.MustCompile(`^<\?xml[^>]*\bencoding\s*=\s*["']([^"']*)["']`)
var utf8Name = map[string]bool{"utf-8": true, "utf8": true, "us-ascii": true, "ascii": true}

// mended is UTF-8 with each byte that isn't (a Latin-1 or Windows-1252 character) as the
// character it is there.
type mended struct {
	r       *bufio.Reader
	buf     [utf8.UTFMax]byte
	pending []byte
}

func (m *mended) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(m.pending) > 0 {
			k := copy(p[n:], m.pending)
			m.pending = m.pending[k:]
			n += k
			continue
		}
		c, err := m.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if c < utf8.RuneSelf {
			p[n] = c
			n++
			continue
		}
		m.r.UnreadByte()
		next, _ := m.r.Peek(utf8.UTFMax) // a character split across reads is whole here
		r, size := utf8.DecodeRune(next)
		if r == utf8.RuneError && size <= 1 {
			if r, size = rune(c), 1; c < 0xa0 {
				r = win1252[c-0x80]
			}
			m.pending = utf8.AppendRune(m.buf[:0], r)
		} else {
			m.pending = append(m.buf[:0], next[:size]...)
		}
		m.r.Discard(size)
	}
	return n, nil
}

type controls struct{ r io.Reader }

func (c *controls) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	for i, b := range p[:n] {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			p[i] = ' '
		}
	}
	return n, err
}

// win1252 is Windows-1252's 0x80-0x9f; the rest of the byte range is Latin-1's code point.
var win1252 = [32]rune{0x20ac, 0x81, 0x201a, 0x192, 0x201e, 0x2026, 0x2020, 0x2021, 0x2c6, 0x2030, 0x160,
	0x2039, 0x152, 0x8d, 0x17d, 0x8f, 0x90, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x2dc,
	0x2122, 0x161, 0x203a, 0x153, 0x9d, 0x17e, 0x178}

func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "utf-8", "utf8", "us-ascii", "ascii":
		return in, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1", "windows-1252", "cp1252", "iso-8859-15":
		// Browsers read Latin-1 as Windows-1252 too: its 0x80-0x9f are controls nobody means.
		return &latin{r: bufio.NewReader(in)}, nil
	}
	return nil, fmt.Errorf("unsupported character set %q", label)
}

type latin struct {
	r       *bufio.Reader
	pending []byte
}

func (l *latin) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(l.pending) > 0 {
			k := copy(p[n:], l.pending)
			l.pending = l.pending[k:]
			n += k
			continue
		}
		b, err := l.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		r := rune(b)
		if b >= 0x80 && b < 0xa0 {
			r = win1252[b-0x80]
		}
		if r < utf8.RuneSelf {
			p[n] = b
			n++
			continue
		}
		var buf [4]byte
		l.pending = buf[:utf8.EncodeRune(buf[:], r)]
	}
	return n, nil
}
