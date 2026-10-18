package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// dib makes a packed DIB with a BITMAPINFOHEADER, then extra bytes (masks)
// and pixels.
func dib(width, height int32, bits uint16, compression uint32, extra, pixels []byte) []byte {
	h := make([]byte, 40)
	binary.LittleEndian.PutUint32(h[0:], 40)
	binary.LittleEndian.PutUint32(h[4:], uint32(width))
	binary.LittleEndian.PutUint32(h[8:], uint32(height))
	binary.LittleEndian.PutUint16(h[12:], 1)
	binary.LittleEndian.PutUint16(h[14:], bits)
	binary.LittleEndian.PutUint32(h[16:], compression)
	return append(append(h, extra...), pixels...)
}

func pixel(t *testing.T, img image.Image, x, y int) color.NRGBA {
	t.Helper()
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

func TestPNGToDIBAndBack(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{G: 255, A: 128})
	src.SetNRGBA(2, 1, color.NRGBA{B: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	d, err := pngToDIB(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	img, err := dibToImage(d)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != src.Bounds() {
		t.Fatalf("bounds %v, want %v", img.Bounds(), src.Bounds())
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			if got, want := pixel(t, img, x, y), src.NRGBAAt(x, y); got != want {
				t.Errorf("pixel %d,%d = %v, want %v", x, y, got, want)
			}
		}
	}
}

// Rows are padded to 4 bytes, and a negative height means top-down.
func TestTopDown24Bit(t *testing.T) {
	// 3 pixels of 3 bytes and 3 bytes of padding per row, in BGR.
	pixels := []byte{
		0, 0, 255 /**/, 0, 255, 0 /**/, 255, 0, 0 /**/, 0, 0, 0,
		1, 2, 3 /**/, 4, 5, 6 /**/, 7, 8, 9 /**/, 0, 0, 0,
	}
	img, err := dibToImage(dib(3, -2, 24, biRGB, nil, pixels))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		x, y int
		want color.NRGBA
	}{
		{0, 0, color.NRGBA{R: 255, A: 255}},
		{1, 0, color.NRGBA{G: 255, A: 255}},
		{2, 0, color.NRGBA{B: 255, A: 255}},
		{2, 1, color.NRGBA{R: 9, G: 8, B: 7, A: 255}},
	} {
		if got := pixel(t, img, c.x, c.y); got != c.want {
			t.Errorf("pixel %d,%d = %v, want %v", c.x, c.y, got, c.want)
		}
	}
}

// Many apps put 32-bit DIBs with 0 in every alpha byte: they are opaque.
func TestZeroAlphaMeansOpaque(t *testing.T) {
	img, err := dibToImage(dib(1, 1, 32, biRGB, nil, []byte{10, 20, 30, 0}))
	if err != nil {
		t.Fatal(err)
	}
	if got := pixel(t, img, 0, 0); got != (color.NRGBA{R: 30, G: 20, B: 10, A: 255}) {
		t.Errorf("pixel = %v", got)
	}
}

// With BI_BITFIELDS, 3 masks follow a 40-byte header.
func TestBitfieldsSkipTheMasks(t *testing.T) {
	masks := make([]byte, 12)
	img, err := dibToImage(dib(1, 1, 32, biBitfields, masks, []byte{10, 20, 30, 255}))
	if err != nil {
		t.Fatal(err)
	}
	if got := pixel(t, img, 0, 0); got != (color.NRGBA{R: 30, G: 20, B: 10, A: 255}) {
		t.Errorf("pixel = %v", got)
	}
}

func TestBadDIBs(t *testing.T) {
	for name, d := range map[string][]byte{
		"too short":  make([]byte, 39),
		"16 bits":    dib(1, 1, 16, biRGB, nil, []byte{0, 0, 0, 0}),
		"compressed": dib(1, 1, 32, 1, nil, []byte{0, 0, 0, 0}),
		"no width":   dib(0, 1, 32, biRGB, nil, nil),
		"cut short":  dib(2, 2, 32, biRGB, nil, make([]byte, 12)),
		"not a DIB":  []byte("not a bitmap, but long enough to have a header of 40 bytes"),
	} {
		if _, err := dibToImage(d); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
