package tag

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"io"
)

// A podcast's cover is often a 3000 px PNG (Apple's largest), 36 MB decoded whole: more than the
// Q2 has to spare. pngShrink reads one a row at a time instead, each row added to the cover row its
// pixels fall in, so it needs two source rows and one cover row of sums.

var errPNG = errors.New("not a PNG this reads")

// pngShrink is a non-interlaced PNG of 8 or 16 bits a sample, shrunk as shrink would to at most
// side pixels a side; transparency is left out (a cover is opaque).
func pngShrink(b []byte, side int) (image.Image, error) {
	if len(b) < 8 || string(b[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, errPNG
	}
	var w, h, depth, kind int
	var palette []byte
	var idat []io.Reader
	for p := b[8:]; len(p) >= 12; {
		n := int(binary.BigEndian.Uint32(p))
		if n < 0 || n > len(p)-12 { // not 12+n > len: that overflows a 32-bit int
			return nil, errPNG
		}
		typ, data := string(p[4:8]), p[8:8+n]
		if crc32.ChecksumIEEE(p[4:8+n]) != binary.BigEndian.Uint32(p[8+n:]) {
			return nil, errPNG
		}
		switch typ {
		case "IHDR":
			if n < 13 || data[10] != 0 || data[11] != 0 || data[12] != 0 { // deflate, filtering 0, not interlaced
				return nil, errPNG
			}
			w, h = int(binary.BigEndian.Uint32(data)), int(binary.BigEndian.Uint32(data[4:]))
			depth, kind = int(data[8]), int(data[9])
		case "PLTE":
			palette = data
		case "IDAT":
			idat = append(idat, bytes.NewReader(data))
		}
		p = p[12+n:]
	}
	channels := map[int]int{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[kind]
	if w < 1 || h < 1 || channels == 0 || (depth != 8 && (depth != 16 || kind == 3)) || (kind == 3 && len(palette) < 3) ||
		w > 1<<14 || h > 1<<14 {
		return nil, errPNG
	}
	z, err := zlib.NewReader(io.MultiReader(idat...))
	if err != nil {
		return nil, errPNG
	}
	defer z.Close()
	bpp := channels * depth / 8 // bytes a pixel, which filters step by
	stride := w * bpp
	dw, dh := fit(w, h, side)
	colOf := cells(w, dw)
	rowOf := cells(h, dh)
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sums, counts := make([]uint32, 3*dw), make([]uint32, dw)
	cur, prev := make([]byte, 1+stride), make([]byte, 1+stride)
	rgb := func(px []byte) (r, g, bl byte) { // a pixel's first byte of each sample
		s := depth / 8
		switch kind {
		case 0, 4:
			return px[0], px[0], px[0]
		case 3:
			i := 3 * int(px[0])
			if i+2 >= len(palette) {
				return 0, 0, 0
			}
			return palette[i], palette[i+1], palette[i+2]
		}
		return px[0], px[s], px[2*s]
	}
	flush := func(oy int) {
		for ox := 0; ox < dw; ox++ {
			n := max(counts[ox], 1)
			o := out.PixOffset(ox, oy)
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = byte(sums[3*ox]/n), byte(sums[3*ox+1]/n), byte(sums[3*ox+2]/n), 0xff
			sums[3*ox], sums[3*ox+1], sums[3*ox+2], counts[ox] = 0, 0, 0, 0
		}
	}
	for y := 0; y < h; y++ {
		if _, err := io.ReadFull(z, cur); err != nil {
			return nil, errPNG
		}
		if !unfilter(cur[0], cur[1:], prev[1:], bpp) {
			return nil, errPNG
		}
		for x := 0; x < w; x++ {
			r, g, bl := rgb(cur[1+x*bpp:])
			ox := colOf[x]
			sums[3*ox] += uint32(r)
			sums[3*ox+1] += uint32(g)
			sums[3*ox+2] += uint32(bl)
			counts[ox]++
		}
		if y == h-1 || rowOf[y+1] != rowOf[y] {
			flush(rowOf[y])
		}
		cur, prev = prev, cur
	}
	return out, nil
}

// unfilter undoes a row's PNG filter in place, given the row above (zeros above the first).
func unfilter(f byte, row, up []byte, bpp int) bool {
	switch f {
	case 0:
	case 1:
		for i := bpp; i < len(row); i++ {
			row[i] += row[i-bpp]
		}
	case 2:
		for i := range row {
			row[i] += up[i]
		}
	case 3:
		for i := range row {
			var left byte
			if i >= bpp {
				left = row[i-bpp]
			}
			row[i] += byte((int(left) + int(up[i])) / 2)
		}
	case 4:
		for i := range row {
			var a, c byte
			if i >= bpp {
				a, c = row[i-bpp], up[i-bpp]
			}
			row[i] += paeth(a, up[i], c)
		}
	default:
		return false
	}
	return true
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
