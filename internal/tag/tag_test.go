package tag

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var audio = bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64, 1, 2, 3, 4}, 5000)

// frames reads a tag's frames back: id to body.
func frames(t *testing.T, b []byte) (map[string][]byte, byte, int) {
	t.Helper()
	if string(b[:3]) != "ID3" {
		t.Fatal("no tag")
	}
	ver, size := b[3], int(syncsafe(b[6:10]))
	body, out := b[10:10+size], map[string][]byte{}
	for len(body) >= 10 && body[0] != 0 {
		n := int(binary.BigEndian.Uint32(body[4:8]))
		if ver == 4 {
			n = int(syncsafe(body[4:8]))
		}
		out[string(body[:4])] = body[10 : 10+n]
		body = body[10+n:]
	}
	return out, ver, 10 + size
}

func info() Info {
	return Info{Title: "Episode 1: Hello", Show: "The Show", Author: "Ann", Date: time.Date(2024, 10, 7, 9, 0, 0, 0, time.UTC)}
}

func TestUntagged(t *testing.T) {
	h, err := ReadHead(bufio.NewReader(bytes.NewReader(audio)))
	if err != nil || !h.Needs(false) || h.Len != 0 {
		t.Fatal(err, h)
	}
	out := h.Build(info(), []byte{0xff, 0xd8, 0xff, 0xd9})
	f, ver, n := frames(t, out)
	if ver != 4 || string(f["TIT2"]) != "\x03Episode 1: Hello" || string(f["TPE1"]) != "\x03Ann" ||
		string(f["TALB"]) != "\x03The Show" || string(f["TCON"]) != "\x03Podcast" || string(f["TDRC"]) != "\x032024-10-07" ||
		!bytes.HasSuffix(f["APIC"], []byte{0xff, 0xd8, 0xff, 0xd9}) || n != len(out) {
		t.Fatalf("%q", f)
	}
}

// A publisher's v2.3 tag keeps its frames (its title, its chapters); what's missing is added as v2.3
// frames.
func TestMerged(t *testing.T) {
	frame := func(id string, body []byte) []byte {
		f := append([]byte(id), 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint32(f[4:8], uint32(len(body)))
		return append(f, body...)
	}
	body := append(frame("TIT2", []byte("\x00Their Title")), frame("CHAP", []byte("ch0\x00\x00\x00\x00\x00\x00\x00\x03\xe8\xff\xff\xff\xff\xff\xff\xff\xff"))...)
	body = append(body, make([]byte, 100)...)
	tag := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0}
	putSyncsafe(tag[6:10], len(body))
	file := append(append(tag, body...), audio...)
	h, err := ReadHead(bufio.NewReader(bytes.NewReader(file)))
	if err != nil || h.Len != int64(len(tag)+len(body)) || !h.Needs(false) {
		t.Fatal(err)
	}
	i := info()
	i.Author = "Zoë ☕"
	f, ver, _ := frames(t, h.Build(i, nil))
	if ver != 3 || string(f["TIT2"]) != "\x00Their Title" || f["CHAP"] == nil || string(f["TYER"]) != "\x002024" ||
		string(f["TALB"]) != "\x00The Show" || f["TDRC"] != nil || f["APIC"] != nil {
		t.Fatalf("%q", f)
	}
	if p := f["TPE1"]; p[0] != 1 || !bytes.Equal(p[1:3], []byte{0xff, 0xfe}) || len(p) != 3+2*5 { // UTF-16
		t.Fatalf("%q", p)
	}
	// The frames the Q2 shows come first, theirs and the new ones, the rest after.
	out := h.Build(i, nil)
	if t2, chap := bytes.Index(out, []byte("TIT2")), bytes.Index(out, []byte("CHAP")); t2 != 10 || chap < bytes.Index(out, []byte("TALB")) {
		t.Fatalf("TIT2 at %d, CHAP at %d", t2, chap)
	}
}

// A tag this package doesn't read (v2.2) is kept whole, after the new one.
func TestKeptWhole(t *testing.T) {
	old := []byte{'I', 'D', '3', 2, 0, 0, 0, 0, 0, 12, 'T', 'T', '2', 0, 0, 6, 0, 'O', 'l', 'd', '!', 0}
	file := append(append([]byte{}, old...), audio...)
	h, err := ReadHead(bufio.NewReader(bytes.NewReader(file)))
	if err != nil {
		t.Fatal(err)
	}
	out := h.Build(info(), nil)
	f, _, n := frames(t, out)
	if f["TIT2"] == nil || !bytes.Equal(out[n:], old) {
		t.Fatalf("%q", out[n:])
	}
}

func TestNotMP3(t *testing.T) {
	for _, b := range [][]byte{[]byte("\x00\x00\x00\x20ftypM4A "), bytes.Repeat([]byte("ep1-"), 5000)} {
		if _, err := ReadHead(bufio.NewReader(bytes.NewReader(b))); err != ErrNotMP3 {
			t.Error(err)
		}
	}
}

// Write keeps the audio as it is, after the new tag; a second look needs nothing.
func TestWrite(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a.part"), filepath.Join(dir, "a.mp3")
	os.WriteFile(src, audio, 0o644)
	if err := Write(src, dst, info(), nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	_, _, n := frames(t, got)
	if !bytes.Equal(got[n:], audio) {
		t.Fatal("audio changed")
	}
	if h, err := Read(dst); err != nil || h.Needs(false) {
		t.Fatal(err)
	}
}

func TestShrink(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 1500, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 1500; x++ {
			big.Set(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var j, p bytes.Buffer
	jpeg.Encode(&j, big, nil)
	png.Encode(&p, big)
	for name, b := range map[string][]byte{"jpeg": j.Bytes(), "png": p.Bytes()} {
		out := Shrink(b)
		cfg, kind, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || kind != "jpeg" || cfg.Width != 600 || cfg.Height != 400 {
			t.Errorf("%s: %v %s %+v", name, err, kind, cfg)
		}
	}
	if Shrink([]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")) != nil {
		t.Error("WebP taken")
	}
}

// A PNG is shrunk a row at a time to what shrinking it whole gives, whatever its kind; one past 4
// megapixels (Apple's 3000 px art) too.
func TestPNGRows(t *testing.T) {
	const w, h = 1500, 1100
	rgb := image.NewNRGBA(image.Rect(0, 0, w, h))
	gray := image.NewGray(rgb.Rect)
	gray16 := image.NewRGBA64(rgb.Rect)
	pal := image.NewPaletted(rgb.Rect, nil)
	for i := 0; i < 256; i++ {
		pal.Palette = append(pal.Palette, color.RGBA{uint8(i), uint8(255 - i), uint8(i * 7), 255})
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(x*7 + y*3 + x*y/97) // detail, so each of PNG's filters is chosen somewhere
			rgb.SetNRGBA(x, y, color.NRGBA{v, uint8(y), uint8(x), 255})
			gray.SetGray(x, y, color.Gray{v})
			gray16.SetRGBA64(x, y, color.RGBA64{uint16(v) << 8, uint16(y) << 6, uint16(x) << 5, 0xffff})
			pal.SetColorIndex(x, y, v)
		}
	}
	for name, img := range map[string]image.Image{"rgb": rgb, "gray": gray, "16-bit": gray16, "palette": pal} {
		var b bytes.Buffer
		png.Encode(&b, img)
		got, err := pngShrink(b.Bytes(), 600)
		if err != nil {
			t.Fatal(name, err)
		}
		want := shrink(img, 600)
		if got.Bounds() != image.Rect(0, 0, 600, 440) {
			t.Fatal(name, got.Bounds())
		}
		for y := 0; y < 440; y += 7 {
			for x := 0; x < 600; x += 7 {
				r1, g1, b1, _ := got.At(x, y).RGBA()
				r2, g2, b2, _ := want.At(x, y).RGBA()
				if d := max(diff(r1, r2), diff(g1, g2), diff(b1, b2)); d > 2<<8 {
					t.Fatalf("%s at %d,%d: %v, want %v", name, x, y, got.At(x, y), want.At(x, y))
				}
			}
		}
	}
	big := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	var b bytes.Buffer
	png.Encode(&b, big)
	if out := Shrink(b.Bytes()); out == nil {
		t.Fatal("a 3000 px PNG refused")
	} else if cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out)); cfg.Width != 600 {
		t.Fatal(cfg)
	}
}

func diff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// A tag broken anywhere is kept whole, never a panic: an extended header without room for its size,
// or a size past a 32-bit int's end (an int is 32 bits on the Q2).
func TestMalformed(t *testing.T) {
	tagged := func(flags byte, body []byte) []byte {
		b := []byte{'I', 'D', '3', 3, 0, flags, 0, 0, 0, 0}
		putSyncsafe(b[6:10], len(body))
		return append(append(b, body...), audio...)
	}
	frame := func(size uint32) []byte { // claiming size bytes, with 4
		return append(binary.BigEndian.AppendUint32([]byte("TIT2"), size), 0, 0, 1, 2, 3, 4)
	}
	ext := func(size uint32) []byte { return append(binary.BigEndian.AppendUint32(nil, size), make([]byte, 6)...) }
	for name, file := range map[string][]byte{
		"no extended header":     tagged(0x40, nil), // ID3 03 00 40 00 00 00 00
		"short extended header":  tagged(0x40, []byte{0, 0}),
		"extended header 2^32-1": tagged(0x40, append(ext(0xffffffff), frame(4)...)),
		"extended header 2^31-4": tagged(0x40, append(ext(0x7ffffffc), frame(4)...)),
		"extended header 2^32-4": tagged(0x40, append(ext(0xfffffffc), frame(4)...)),
		"frame 2^31-1":           tagged(0, frame(0x7fffffff)),
		"frame 2^31-10":          tagged(0, frame(0x7ffffff6)),
		"frame 2^32-1":           tagged(0, frame(0xffffffff)),
	} {
		h, err := ReadHead(bufio.NewReader(bytes.NewReader(file)))
		if err != nil || len(h.after) != 1 || len(h.frames) != 0 || !h.Needs(false) {
			t.Errorf("%s: %v %+v", name, err, h)
		}
	}
	if h, err := ReadHead(bufio.NewReader(bytes.NewReader(tagged(0x40, append(ext(6), frame(4)...))))); err != nil ||
		len(h.after) != 0 || !h.has["TIT2"] {
		t.Errorf("a sound extended header: %v %+v", err, h)
	}
	for _, n := range []uint32{0x7ffffff4, 0x7fffffff, 0xffffffff} {
		b := binary.BigEndian.AppendUint32([]byte("\x89PNG\r\n\x1a\n"), n)
		b = append(append(b, "IHDR"...), make([]byte, 17)...)
		if _, err := pngShrink(b, 600); err != errPNG {
			t.Errorf("PNG chunk of %#x: %v", n, err)
		}
	}
}

// A tag past maxTag isn't read, and says so: a new tag would lose it.
func TestBig(t *testing.T) {
	head := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0}
	putSyncsafe(head[6:10], maxTag)
	file := append(append(head, make([]byte, maxTag)...), audio...)
	h, err := ReadHead(bufio.NewReader(bytes.NewReader(file)))
	if err != nil || !h.Big() || h.Len != int64(len(head)+maxTag) {
		t.Fatal(err, h)
	}
	if h, _ := ReadHead(bufio.NewReader(bytes.NewReader(audio))); h.Big() {
		t.Fatal("untagged is big")
	}
}

// progJPEG is a w by h grey progressive JPEG: one scan, of its blocks' DC coefficients (all 0), a bit
// a block, so it is small however big. SOI; DQT, table 0 all ones; SOF2, one component; DHT, DC
// table 0 with one code a bit long, for 0; SOS, the DC scan.
func progJPEG(w, h int) []byte {
	b := append([]byte{0xff, 0xd8, 0xff, 0xdb, 0, 67, 0}, bytes.Repeat([]byte{1}, 64)...)
	b = append(b, 0xff, 0xc2, 0, 11, 8, byte(h>>8), byte(h), byte(w>>8), byte(w), 1, 1, 0x11, 0)
	b = append(append(b, 0xff, 0xc4, 0, 20, 0, 1), make([]byte, 16)...)
	b = append(b, 0xff, 0xda, 0, 8, 1, 1, 0, 0, 0, 0)
	b = append(b, make([]byte, ((w+7)/8*((h+7)/8)+7)/8)...)
	return append(b, 0xff, 0xd9)
}

// A progressive JPEG past 2 megapixels is no cover: Go keeps its coefficients whole as it decodes.
// A smaller one is, and a baseline one that big.
func TestProgressive(t *testing.T) {
	if Shrink(progJPEG(1000, 1000)) == nil {
		t.Fatal("a small progressive JPEG refused")
	}
	big := progJPEG(2000, 1504)
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(big)); err != nil || cfg.Width != 2000 || !progressive(big) {
		t.Fatal(err, cfg)
	}
	if Shrink(big) != nil {
		t.Fatal("a big progressive JPEG taken")
	}
	var j bytes.Buffer
	jpeg.Encode(&j, image.NewGray(image.Rect(0, 0, 2000, 1504)), nil)
	if progressive(j.Bytes()) || Shrink(j.Bytes()) == nil {
		t.Fatal("a big baseline JPEG refused")
	}
	// Its frame is found as image/jpeg finds it: past an APP1 holding a frame's marker, stray bytes
	// and fill bytes.
	odd := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 6, 0xff, 0xc0, 0, 0, 0x12, 0x34, 0xff}, big[2:]...)
	if _, err := jpeg.DecodeConfig(bytes.NewReader(odd)); err != nil || !progressive(odd) {
		t.Fatal("frame not found:", err)
	}
}
