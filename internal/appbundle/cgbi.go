package appbundle

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
)

// decodeIcon tries the standard registered decoders first (png, jpeg). When
// those fail it falls back to decodeCgBI, because icons lifted straight out
// of iOS apps (and most jailbreak-world assets) ship as Apple's proprietary
// "CgBI" PNG variant, which Go's image/png cannot read.
func decodeIcon(r io.Reader) (image.Image, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err == nil {
		return img, nil
	}
	if !hasCgBIChunk(data) {
		return nil, err
	}
	cg, err := decodeCgBI(data)
	if err != nil {
		return nil, fmt.Errorf("decode CgBI icon: %w", err)
	}
	return cg, nil
}

// hasCgBIChunk reports whether the PNG chunk stream carries Apple's CgBI
// marker (the ancillary chunk that turns a normal PNG into the optimized
// variant only iOS understands).
func hasCgBIChunk(data []byte) bool {
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	for pos := 8; pos+12 <= len(data); {
		ln := int(binary.BigEndian.Uint32(data[pos:]))
		if bytes.Equal(data[pos+4:pos+8], []byte("CgBI")) {
			return true
		}
		pos += 12 + ln
	}
	return false
}

// decodeCgBI decodes Apple's CgBI PNG variant into a plain image. The format
// differs from a standard PNG in three ways, all confirmed empirically
// against Apple's own decoder (sips) on real app icons:
//
//  1. the IDAT stream is raw deflate — no zlib header;
//  2. scanlines keep their per-row filter byte and are otherwise standard
//     PNG-filtered 8-bit RGBA data;
//  3. the actual pixels are stored premultiplied BGRA, not straight RGBA.
//
// Only 8-bit, non-interlaced RGBA (color type 6) files are produced by the
// Apple encoder; anything else is rejected.
func decodeCgBI(data []byte) (image.Image, error) {
	pos := 8
	var w, h int
	haveCgBI := false
	var idat []byte
	for pos+12 <= len(data) {
		ln := int(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		body := data[pos+8 : pos+8+ln]
		switch typ {
		case "CgBI":
			haveCgBI = true
		case "IHDR":
			if len(body) < 13 {
				return nil, fmt.Errorf("short IHDR")
			}
			w = int(binary.BigEndian.Uint32(body[0:]))
			h = int(binary.BigEndian.Uint32(body[4:]))
			if body[8] != 8 || body[9] != 6 || body[12] != 0 {
				return nil, fmt.Errorf("unsupported CgBI layout (want 8-bit RGBA, non-interlaced)")
			}
		case "IDAT":
			idat = append(idat, body...)
		}
		pos += 12 + ln
		if typ == "IEND" {
			break
		}
	}
	if !haveCgBI {
		return nil, fmt.Errorf("not a CgBI PNG")
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("missing IHDR")
	}

	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(idat)))
	if err != nil {
		return nil, err
	}
	stride := w * 4
	if len(raw) != stride*h+h {
		return nil, fmt.Errorf("unexpected CgBI data length %d (want %d)", len(raw), stride*h+h)
	}

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var prev, row []byte
	for y := 0; y < h; y++ {
		f := raw[0]
		raw = raw[1:]
		row = append(row[:0], raw[:stride]...)
		raw = raw[stride:]
		unfilterPNG(row, prev, f)
		prev = append(prev[:0], row...)
		for x := 0; x < w; x++ {
			b, g, r, a := row[x*4], row[x*4+1], row[x*4+2], row[x*4+3]
			if a != 0 { // CgBI is premultiplied; undo it
				r = byte(int(r) * 255 / int(a))
				g = byte(int(g) * 255 / int(a))
				b = byte(int(b) * 255 / int(a))
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}

// unfilterPNG undoes one scanline of PNG filtering into row (the filter type
// is f; prev is the previous row, zeroed for the first).
func unfilterPNG(row, prev []byte, f byte) {
	bpp := 4
	switch f {
	case 0: // None
	case 1: // Sub
		for i := bpp; i < len(row); i++ {
			row[i] += row[i-bpp]
		}
	case 2: // Up
		for i := range row {
			row[i] += prev[i]
		}
	case 3: // Average
		for i := range row {
			var left byte
			if i >= bpp {
				left = row[i-bpp]
			}
			row[i] += byte((int(prev[i]) + int(left)) / 2)
		}
	case 4: // Paeth
		for i := range row {
			var left byte
			if i >= bpp {
				left = row[i-bpp]
			}
			var upLeft byte
			if i >= bpp {
				upLeft = prev[i-bpp]
			}
			row[i] += paeth(left, prev[i], upLeft)
		}
	}
}

// paeth returns the PNG Paeth predictor for a (left), b (up), c (up-left).
func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	default:
		return c
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
