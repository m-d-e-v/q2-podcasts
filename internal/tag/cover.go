package tag

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // covers come as JPEG or PNG (an interlaced PNG decoded whole)
	"io"
	"net/http"
)

const (
	coverPx  = 600      // a side, at most: the Qobuz app's covers, which the Q2 shows
	maxImage = 10 << 20 // bytes of an image fetched
)

// Cover is the image at u as a cover the Q2 shows: a baseline JPEG, coverPx a side at most. nil when
// it can't be had: unreachable, a kind Go doesn't read (WebP), or too big to decode in the Q2's
// memory (a JPEG past 10 megapixels, a progressive one past 2; a PNG is read a row at a time,
// pngShrink).
func Cover(ctx context.Context, c *http.Client, u, agent string) []byte {
	if u == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", agent)
	resp, err := c.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImage))
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	return Shrink(b)
}

// Shrink is an image's bytes as a cover (Cover's), nil when it can't be.
func Shrink(b []byte) []byte {
	cfg, kind, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 {
		return nil
	}
	var img image.Image
	if kind == "png" {
		img, err = pngShrink(b, coverPx)
	}
	if img == nil { // a JPEG; a PNG pngShrink doesn't read (interlaced), if small
		px := int64(cfg.Width) * int64(cfg.Height) // an int is 32 bits on the Q2
		if px > 10_000_000 || kind != "jpeg" && px > 4_000_000 || kind == "jpeg" && px > 2_000_000 && progressive(b) {
			return nil
		}
		if img, _, err = image.Decode(bytes.NewReader(b)); err != nil {
			return nil
		}
		img = shrink(img, coverPx)
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}) != nil {
		return nil
	}
	return out.Bytes()
}

// progressive is whether a JPEG's frame (SOF2, 6, 10 or 14) is progressive, its markers read up to it
// as image/jpeg reads them. Go keeps a progressive JPEG's coefficients until its last scan, 4 bytes a
// pixel and colour: 67 MB for 3000 px.
func progressive(b []byte) bool {
	for i := 2; i+3 < len(b); {
		m := b[i+1]
		if b[i] != 0xff || m == 0 || m == 0xff || m >= 0xd0 && m <= 0xd7 { // stray bytes, fill, a restart
			i++
			continue
		}
		switch m {
		case 0xc2, 0xc6, 0xca, 0xce:
			return true
		case 0xc0, 0xc1, 0xc3, 0xc5, 0xc7, 0xc9, 0xcb, 0xcd, 0xcf: // another frame
			return false
		case 0xd9, 0xda: // the end, or a scan, before a frame
			return false
		}
		i += 2 + int(binary.BigEndian.Uint16(b[i+2:]))
	}
	return false
}

// fit is a w by h image's size at most side pixels a side.
func fit(w, h, side int) (int, int) {
	switch {
	case w <= side && h <= side:
		return w, h
	case h > w:
		return max(1, w*side/h), side
	}
	return side, max(1, h*side/w)
}

// box is the source pixels [from, to) that pixel o of d covers, of n.
func box(o, n, d int) (int, int) { return o * n / d, max((o+1)*n/d, o*n/d+1) }

// cells is, for each of n source pixels, the one of d it falls in.
func cells(n, d int) []int {
	out := make([]int, n)
	for o := 0; o < d; o++ {
		a, b := box(o, n, d)
		for i := a; i < b && i < n; i++ {
			out[i] = o
		}
	}
	return out
}

// shrink is img at most side pixels a side, each pixel the average of those it covers.
func shrink(img image.Image, side int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	dw, dh := fit(w, h, side)
	if y, ok := img.(*image.YCbCr); ok { // a JPEG's planes averaged as they are: no RGB per pixel
		out := image.NewYCbCr(image.Rect(0, 0, dw, dh), image.YCbCrSubsampleRatio444)
		for oy := 0; oy < dh; oy++ {
			y0, y1 := box(oy, h, dh)
			for ox := 0; ox < dw; ox++ {
				x0, x1 := box(ox, w, dw)
				var sy, scb, scr, n int
				for sy0 := y0; sy0 < y1; sy0++ {
					for sx := x0; sx < x1; sx++ {
						px, py := b.Min.X+sx, b.Min.Y+sy0
						ci := y.COffset(px, py)
						sy += int(y.Y[y.YOffset(px, py)])
						scb += int(y.Cb[ci])
						scr += int(y.Cr[ci])
						n++
					}
				}
				i := out.YOffset(ox, oy)
				out.Y[i], out.Cb[i], out.Cr[i] = uint8(sy/n), uint8(scb/n), uint8(scr/n)
			}
		}
		return out
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for oy := 0; oy < dh; oy++ {
		y0, y1 := box(oy, h, dh)
		for ox := 0; ox < dw; ox++ {
			x0, x1 := box(ox, w, dw)
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := img.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r, g, bl, a, n = r+cr, g+cg, bl+cb, a+ca, n+1
				}
			}
			out.SetRGBA(ox, oy, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return out
}
