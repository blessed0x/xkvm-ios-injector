package appbundle

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/plist"
	"github.com/xscope0/xkvm-ios-injector/internal/testutil"
)

// writeCgBI encodes img as an Apple CgBI PNG — the exact format the decoder
// reverses: raw-deflate IDAT (no zlib header), one filter byte per row, and
// premultiplied BGRA pixels — with the CgBI marker chunk up front. This is a
// hermetic fixture: no app assets in the repo, and it exercises every step
// of decodeCgBI including the pixel transform.
func writeCgBI(t *testing.T, path string, img image.Image, w, h int) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	writeChunk := func(typ string, body []byte) {
		var b bytes.Buffer
		binary.Write(&b, binary.BigEndian, uint32(len(body)))
		b.WriteString(typ)
		b.Write(body)
		crc := crc32.NewIEEE()
		crc.Write(b.Bytes()[4:])
		binary.Write(&b, binary.BigEndian, crc.Sum32())
		buf.Write(b.Bytes())
	}
	writeChunk("CgBI", []byte{0x50, 0x00, 0x20, 0x06})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9], ihdr[12] = 8, 6, 0 // 8-bit, RGBA, non-interlaced
	writeChunk("IHDR", ihdr)

	var scan bytes.Buffer
	for y := 0; y < h; y++ {
		scan.WriteByte(0) // filter: None
		for x := 0; x < w; x++ {
			// Undo the premultiply color.Color.RGBA() applies (its contract is
			// premultiplied alpha) to recover straight channels, then premultiply
			// exactly once here, like Apple's compressor does.
			r, g, b, a := img.At(x, y).RGBA()
			if a != 0 { // undo premultiply, staying on the 16-bit scale
				r = r * 0xffff / a
				g = g * 0xffff / a
				b = b * 0xffff / a
			}
			r8, g8, b8, a8 := byte(r>>8), byte(g>>8), byte(b>>8), byte(a>>8)
			if a8 > 0 { // premultiply
				r8 = byte(int(r8) * int(a8) / 255)
				g8 = byte(int(g8) * int(a8) / 255)
				b8 = byte(int(b8) * int(a8) / 255)
			}
			scan.Write([]byte{b8, g8, r8, a8}) // BGRA
		}
	}
	var comp bytes.Buffer
	zw, _ := flate.NewWriter(&comp, flate.DefaultCompression)
	if _, err := zw.Write(scan.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeChunk("IDAT", comp.Bytes())
	writeChunk("IEND", nil)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeCgBI(t *testing.T) {
	w, h := 8, 4
	// NRGBA is straight alpha: the encoder must be the one to premultiply,
	// exactly like Apple's compressor does. (image.RGBA would hand back
	// already-premultiplied values and double the transform.)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.SetNRGBA(0, 0, color.NRGBA{200, 100, 50, 255})  // opaque
	img.SetNRGBA(1, 0, color.NRGBA{100, 150, 200, 128}) // half-transparent
	img.SetNRGBA(2, 0, color.NRGBA{0, 0, 0, 0})         // fully transparent
	img.SetNRGBA(3, 0, color.NRGBA{250, 10, 20, 255})   // opaque saturated

	cgbiPath := filepath.Join(t.TempDir(), "icon-cgbi.png")
	writeCgBI(t, cgbiPath, img, w, h)

	data, err := os.ReadFile(cgbiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hasCgBIChunk(data) {
		t.Fatal("hasCgBIChunk false on a CgBI file")
	}

	decoded, err := decodeCgBI(data)
	if err != nil {
		t.Fatalf("decodeCgBI: %v", err)
	}

	check := func(x, y int, want color.RGBA, tol int) {
		t.Helper()
		c := decoded.At(x, y).(color.NRGBA)
		got := color.RGBA(c)
		for _, ch := range []struct {
			got, want uint8
		}{
			{got.R, want.R}, {got.G, want.G}, {got.B, want.B}, {got.A, want.A},
		} {
			if diff := abs(int(ch.got) - int(ch.want)); diff > tol {
				t.Errorf("pixel (%d,%d) = %+v, want %+v (tol %d)", x, y, got, want, tol)
				break
			}
		}
	}
	check(0, 0, color.RGBA{200, 100, 50, 255}, 0)  // opaque survives exactly
	check(1, 0, color.RGBA{100, 150, 200, 128}, 1) // premultiply/unpremultiply round-trips to ±1
	check(2, 0, color.RGBA{0, 0, 0, 0}, 0)         // transparent stays
	check(3, 0, color.RGBA{250, 10, 20, 255}, 0)   // opaque saturated

	// A standard PNG must NOT be detected as CgBI and must decode via the
	// normal path (no fallback).
	stdPath := filepath.Join(t.TempDir(), "std.png")
	if err := writeStandardPNG(stdPath, img, w, h); err != nil {
		t.Fatal(err)
	}
	stdData, err := os.ReadFile(stdPath)
	if err != nil {
		t.Fatal(err)
	}
	if hasCgBIChunk(stdData) {
		t.Fatal("hasCgBIChunk true on a standard PNG")
	}
	if _, err := decodeIcon(bytes.NewReader(stdData)); err != nil {
		t.Fatalf("decodeIcon on standard PNG: %v", err)
	}
}

// TestChangeIconCgBI drives the full ChangeIcon path with a CgBI source —
// the exact failure the TUI hit with a real iOS app icon — and verifies the
// produced icons are valid standard PNGs readable by image.Decode.
func TestChangeIconCgBI(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	appDir := testutil.MakeApp(t, tmp, "TestApp", "com.example.test")

	img := image.NewRGBA(image.Rect(0, 0, 120, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{30, 90, 220, 255})
		}
	}
	cgbiPath := filepath.Join(tmp, "icon-cgbi.png")
	writeCgBI(t, cgbiPath, img, 120, 120)

	b := openBundle(t, appDir)
	if err := b.ChangeIcon(cgbiPath); err != nil {
		t.Fatalf("ChangeIcon with CgBI source: %v", err)
	}

	entries, err := os.ReadDir(appDir)
	if err != nil {
		t.Fatal(err)
	}
	var pngs []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			pngs = append(pngs, e.Name())
		}
	}
	if len(pngs) != 2 {
		t.Fatalf("expected 2 icon PNGs, got %v", pngs)
	}
	// The produced icons must be standard PNGs (image.Decode reads them).
	for _, n := range pngs {
		f, err := os.Open(filepath.Join(appDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := image.Decode(f); err != nil {
			t.Errorf("produced icon %s is not a standard PNG: %v", n, err)
		}
		f.Close()
	}

	info, err := plist.Open(filepath.Join(appDir, "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if icons, _ := info["CFBundleIcons"].(map[string]any); icons == nil {
		t.Error("CFBundleIcons missing after CgBI icon change")
	}
}

func writeStandardPNG(path string, img image.Image, w, h int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
