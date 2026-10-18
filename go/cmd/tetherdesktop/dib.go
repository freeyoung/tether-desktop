package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
)

// The Windows clipboard holds bitmaps as DIBs, and the engine syncs PNGs.
// This file converts between them. It is plain Go, so its tests run on any
// OS, not only on Windows.

const (
	biRGB       = 0
	biBitfields = 3
)

// dibToImage reads a packed DIB (BITMAPINFOHEADER and pixels) of 24 or 32
// bits per pixel, the forms screenshots and image apps put.
func dibToImage(dib []byte) (image.Image, error) {
	if len(dib) < 40 {
		return nil, errors.New("DIB too short")
	}
	headerSize := int(binary.LittleEndian.Uint32(dib[0:]))
	width := int(int32(binary.LittleEndian.Uint32(dib[4:])))
	height := int(int32(binary.LittleEndian.Uint32(dib[8:])))
	bits := int(binary.LittleEndian.Uint16(dib[14:]))
	compression := binary.LittleEndian.Uint32(dib[16:])
	colorsUsed := int(binary.LittleEndian.Uint32(dib[32:]))
	if width <= 0 || height == 0 || (bits != 24 && bits != 32) || (compression != biRGB && compression != biBitfields) {
		return nil, errors.New("unsupported DIB format")
	}
	offset := headerSize + colorsUsed*4
	if compression == biBitfields && headerSize == 40 {
		offset += 12 // the 3 masks follow a plain BITMAPINFOHEADER
	}
	topDown := height < 0
	if topDown {
		height = -height
	}
	stride := ((width*bits + 31) / 32) * 4
	if offset+stride*height > len(dib) {
		return nil, errors.New("DIB pixels are cut short")
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	// 32-bit DIBs often carry 0 in every alpha byte: then they are opaque.
	anyAlpha := false
	if bits == 32 {
		for i := offset + 3; i < offset+stride*height; i += 4 {
			if dib[i] != 0 {
				anyAlpha = true
				break
			}
		}
	}
	for y := 0; y < height; y++ {
		row := y
		if !topDown {
			row = height - 1 - y
		}
		line := dib[offset+row*stride:]
		for x := 0; x < width; x++ {
			p := line[x*bits/8:]
			a := uint8(255)
			if bits == 32 && anyAlpha {
				a = p[3]
			}
			img.SetNRGBA(x, y, color.NRGBA{R: p[2], G: p[1], B: p[0], A: a})
		}
	}
	return img, nil
}

// pngToDIB makes a 32-bit bottom-up DIB, which every app that reads bitmaps
// can paste.
func pngToDIB(data []byte) ([]byte, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	out := make([]byte, 40+width*height*4)
	binary.LittleEndian.PutUint32(out[0:], 40)
	binary.LittleEndian.PutUint32(out[4:], uint32(width))
	binary.LittleEndian.PutUint32(out[8:], uint32(height))
	binary.LittleEndian.PutUint16(out[12:], 1)
	binary.LittleEndian.PutUint16(out[14:], 32)
	binary.LittleEndian.PutUint32(out[16:], biRGB)
	binary.LittleEndian.PutUint32(out[20:], uint32(width*height*4))
	for y := 0; y < height; y++ {
		line := out[40+(height-1-y)*width*4:]
		for x := 0; x < width; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			p := line[x*4:]
			p[0], p[1], p[2], p[3] = c.B, c.G, c.R, c.A
		}
	}
	return out, nil
}
