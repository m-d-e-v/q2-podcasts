// Package tag fills in what the Q2 shows of an episode in its file: an MP3's ID3v2 tag, which
// TagLib reads for Now Playing, gets the episode's title, the show, its author, the date and the
// cover where the publisher left them out (Patreon's files carry none). Everything the publisher
// wrote is kept as it is, chapters among it, which Q2 Pod's Chapters reads through FFmpeg.
package tag

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf16"
)

// Info is an episode as its tag names it.
type Info struct {
	Title, Show, Author string
	Date                time.Time
}

var ErrNotMP3 = errors.New("not an MP3 file")

const (
	maxTag  = 8 << 20 // a tag past this isn't read into memory: its file is left as it is (Big)
	padding = 1024    // room in a new tag, for a tag editor's later change
	scan    = 8 << 10 // an MPEG frame's start is looked for this far past the tags
)

// Head is an MP3's leading ID3v2 tags as read. The first one's frames, when it is one this package
// reads (v2.3 or v2.4, not unsynchronised), go into the new tag; any other is kept whole after it.
type Head struct {
	Len     int64 // the bytes the tags take
	version byte
	frames  [][]byte // each frame whole, its header too
	has     map[string]bool
	after   [][]byte
	big     bool // a tag past maxTag, passed over
}

// ReadHead reads r's leading ID3v2 tags, leaving r at the audio. An MP3 has an MPEG frame's start
// soon after them; anything else is ErrNotMP3.
func ReadHead(r *bufio.Reader) (*Head, error) {
	h := &Head{version: 4, has: map[string]bool{}}
	for first := true; ; first = false {
		b, _ := r.Peek(10)
		if len(b) < 10 || string(b[:3]) != "ID3" {
			break
		}
		size := int64(syncsafe(b[6:10])) + 10
		if b[3] == 4 && b[5]&0x10 != 0 { // a footer
			size += 10
		}
		if size > maxTag {
			if _, err := r.Discard(int(size)); err != nil {
				return nil, err
			}
			h.Len, h.big = h.Len+size, true
			continue
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, err
		}
		h.Len += size
		if first && h.take(raw) {
			continue
		}
		h.after = append(h.after, raw)
	}
	if !mpeg(r) {
		return nil, ErrNotMP3
	}
	return h, nil
}

// mpeg is whether an MPEG audio frame's start (11 bits set) comes within scan bytes.
func mpeg(r *bufio.Reader) bool {
	b, _ := r.Peek(scan)
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xff && b[i+1]&0xe0 == 0xe0 {
			return true
		}
	}
	return false
}

// take reads a tag's frames into h; false when it isn't one this package reads.
func (h *Head) take(raw []byte) bool {
	ver, flags := raw[3], raw[5]
	if (ver != 3 && ver != 4) || flags&0x80 != 0 { // v2.2, or unsynchronised: kept whole
		return false
	}
	body := raw[10 : 10+syncsafe(raw[6:10])]
	if flags&0x40 != 0 { // an extended header, left out
		if len(body) < 4 {
			return false
		}
		n := int64(binary.BigEndian.Uint32(body)) + 4 // int64: an int is 32 bits on the Q2
		if ver == 4 {
			n = int64(syncsafe(body))
		}
		if n > int64(len(body)) {
			return false
		}
		body = body[n:]
	}
	var frames [][]byte
	has := map[string]bool{}
	for len(body) >= 10 && body[0] != 0 {
		id := string(body[:4])
		n := int(binary.BigEndian.Uint32(body[4:8]))
		if ver == 4 {
			n = int(syncsafe(body[4:8]))
		}
		if !frameID(id) || n < 0 || n > len(body)-10 { // not 10+n > len: that overflows a 32-bit int
			return false
		}
		frames = append(frames, body[:10+n])
		has[id] = true
		body = body[10+n:]
	}
	if len(bytes.Trim(body, "\x00")) > 0 { // not padding past the frames
		return false
	}
	h.version, h.frames, h.has = ver, frames, has
	return true
}

func frameID(id string) bool {
	for i := 0; i < 4; i++ {
		if c := id[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Needs is whether the tag lacks what the Q2 shows: a title, an artist or an album, or a cover when
// there is one to give.
func (h *Head) Needs(cover bool) bool {
	return !h.has["TIT2"] || !h.has["TPE1"] || !h.has["TALB"] || cover && !h.has["APIC"]
}

// HasCover is whether the tag has a picture.
func (h *Head) HasCover() bool { return h.has["APIC"] }

// Big is whether a tag was past maxTag: not read, so a new tag would lose it.
func (h *Head) Big() bool { return h.big }

// shown are the frames the Q2 shows, put first in a new tag: ahead of the rest, however big (an
// editor's XMP).
var shown = map[string]bool{"TIT2": true, "TPE1": true, "TALB": true, "TPE2": true, "TCON": true, "TDRC": true,
	"TYER": true, "TRCK": true, "APIC": true}

// Build is the new start of the file: one tag, the publisher's frames the Q2 shows and those missing
// from them, then the rest of theirs, then any tag kept whole.
func (h *Head) Build(i Info, cover []byte) []byte {
	var f bytes.Buffer
	for _, fr := range h.frames {
		if shown[string(fr[:4])] {
			f.Write(fr)
		}
	}
	add := func(id, s string) {
		if s != "" && !h.has[id] {
			f.Write(h.frame(id, h.text(s)))
		}
	}
	add("TIT2", i.Title)
	add("TPE1", cmp.Or(i.Author, i.Show))
	add("TALB", i.Show)
	add("TCON", "Podcast")
	if !i.Date.IsZero() && !h.has["TDRC"] && !h.has["TYER"] {
		if h.version == 4 {
			add("TDRC", i.Date.Format("2006-01-02"))
		} else {
			add("TYER", i.Date.Format("2006"))
		}
	}
	if len(cover) > 0 && !h.has["APIC"] {
		apic := append([]byte{0}, "image/jpeg"...) // ISO-8859-1 for the (empty) description
		apic = append(apic, 0, 3, 0)               // the MIME type's end, a front cover, no description
		f.Write(h.frame("APIC", append(apic, cover...)))
	}
	for _, fr := range h.frames {
		if !shown[string(fr[:4])] {
			f.Write(fr)
		}
	}
	f.Write(make([]byte, padding))
	out := []byte{'I', 'D', '3', h.version, 0, 0, 0, 0, 0, 0}
	putSyncsafe(out[6:10], f.Len())
	out = append(out, f.Bytes()...)
	for _, t := range h.after {
		out = append(out, t...)
	}
	return out
}

// text is a text frame's body: UTF-8 in v2.4; in v2.3, Latin-1 where it will do, else UTF-16.
func (h *Head) text(s string) []byte {
	if h.version == 4 {
		return append([]byte{3}, s...)
	}
	latin := []byte{0}
	for _, r := range s {
		if r > 0xff {
			b := []byte{1, 0xff, 0xfe} // UTF-16 with a byte order mark, little-endian
			for _, u := range utf16.Encode([]rune(s)) {
				b = append(b, byte(u), byte(u>>8))
			}
			return b
		}
		latin = append(latin, byte(r))
	}
	return latin
}

func (h *Head) frame(id string, body []byte) []byte {
	f := make([]byte, 10, 10+len(body))
	copy(f, id)
	if h.version == 4 {
		putSyncsafe(f[4:8], len(body))
	} else {
		binary.BigEndian.PutUint32(f[4:8], uint32(len(body)))
	}
	return append(f, body...)
}

func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 | uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}

func putSyncsafe(b []byte, n int) {
	b[0], b[1], b[2], b[3] = byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f)
}

// Read is the head of the MP3 at path.
func Read(path string) (*Head, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadHead(bufio.NewReaderSize(f, 64<<10))
}

// Write puts the MP3 at src, tagged, at dst: the new tag, then src's audio as it is. dst is written
// whole and synced; the caller renames it into place.
func Write(src, dst string, i Info, cover []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r := bufio.NewReaderSize(in, 64<<10)
	h, err := ReadHead(r)
	if err != nil {
		return err
	}
	head := h.Build(i, cover)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = out.Write(head)
	if err == nil {
		_, err = io.CopyBuffer(out, r, make([]byte, 512<<10))
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
