package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"tetherdesktop/core"
)

// windowsClipboard is the Windows clipboard behind the engine's Clipboard.
// Every call runs on the UI thread, which owns the clipboard window.
type windowsClipboard struct {
	ui        *uiThread
	pngFormat uint32
}

func newWindowsClipboard(ui *uiThread) *windowsClipboard {
	name, _ := windows.UTF16PtrFromString("PNG")
	id, _, _ := pRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(name)))
	return &windowsClipboard{ui: ui, pngFormat: uint32(id)}
}

// ChangeCount may be read on any thread; it changes with every write.
func (c *windowsClipboard) ChangeCount() uint64 {
	n, _, _ := pGetClipboardSequenceNumber.Call()
	return uint64(n)
}

func (c *windowsClipboard) Read() (content core.Content, ok bool) {
	c.ui.do(func() {
		if !c.open() {
			return
		}
		defer pCloseClipboard.Call()
		switch core.ChooseWindows(c.formats()) {
		case core.ChooseText:
			if text, found := c.readText(); found && text != "" {
				content, ok = core.TextContent(text), true
			}
		case core.ChooseImage:
			if img := c.readImage(); img != nil {
				content, ok = core.PNGContent(img), true
			}
		}
	})
	return
}

func (c *windowsClipboard) Write(content core.Content) uint64 {
	c.ui.do(func() {
		if !c.open() {
			logLine("could not open the clipboard to write it")
			return
		}
		defer pCloseClipboard.Call()
		pEmptyClipboard.Call()
		if content.IsPNG {
			// PNG for apps that read it, a DIB for the many that only read that.
			c.set(c.pngFormat, content.PNG)
			if dib, err := pngToDIB(content.PNG); err == nil {
				c.set(core.CF_DIB, dib)
			} else {
				logLine("could not convert the image for the clipboard: " + err.Error())
			}
		} else {
			units := utf16.Encode([]rune(content.Text))
			data := make([]byte, 2*len(units)+2)
			for i, u := range units {
				binary.LittleEndian.PutUint16(data[2*i:], u)
			}
			c.set(core.CF_UNICODETEXT, data)
		}
	})
	return c.ChangeCount()
}

// open opens the clipboard with our window as its owner. Another app may
// hold it for a moment.
func (c *windowsClipboard) open() bool {
	for i := 0; i < 10; i++ {
		if r, _, _ := pOpenClipboard.Call(c.ui.hwnd); r != 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// formats lists the formats in the order their owner set them.
// CanIncludeInClipboardHistory is passed on only when it forbids history
// (value 0), as password managers set it.
func (c *windowsClipboard) formats() []core.WindowsFormat {
	var list []core.WindowsFormat
	buf := make([]uint16, 256)
	for id, _, _ := pEnumClipboardFormats.Call(0); id != 0; id, _, _ = pEnumClipboardFormats.Call(id) {
		n, _, _ := pGetClipboardFormatName.Call(id, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		name := ""
		if n > 0 {
			name = windows.UTF16ToString(buf[:n])
		}
		if name == "CanIncludeInClipboardHistory" {
			if data := c.get(uint32(id)); len(data) >= 4 && binary.LittleEndian.Uint32(data) != 0 {
				continue
			}
		}
		list = append(list, core.WindowsFormat{ID: uint32(id), Name: name})
	}
	return list
}

func (c *windowsClipboard) readText() (string, bool) {
	data := c.get(core.CF_UNICODETEXT)
	if data == nil {
		return "", false
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	return windows.UTF16ToString(units), true
}

func (c *windowsClipboard) readImage() []byte {
	// Browsers and most modern apps put a PNG; keep it as it is.
	if data := c.get(c.pngFormat); len(data) > 8 && bytes.HasPrefix(data, []byte("\x89PNG")) {
		return data
	}
	// Windows makes a CF_DIB from any bitmap format.
	dib := c.get(core.CF_DIB)
	if dib == nil {
		return nil
	}
	img, err := dibToImage(dib)
	if err != nil {
		logLine("could not read the clipboard image: " + err.Error())
		return nil
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil
	}
	return out.Bytes()
}

// get copies the bytes of one format, with the clipboard open.
func (c *windowsClipboard) get(format uint32) []byte {
	handle, _, _ := pGetClipboardData.Call(uintptr(format))
	if handle == 0 {
		return nil
	}
	size, _, _ := pGlobalSize.Call(handle)
	pointer, _, _ := pGlobalLock.Call(handle)
	if pointer == 0 {
		return nil
	}
	defer pGlobalUnlock.Call(handle)
	return bytes.Clone(globalBytes(pointer, int(size)))
}

// set hands the bytes of one format to the clipboard, with it open and empty.
func (c *windowsClipboard) set(format uint32, data []byte) {
	handle, _, _ := pGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if handle == 0 {
		return
	}
	pointer, _, _ := pGlobalLock.Call(handle)
	if pointer == 0 {
		pGlobalFree.Call(handle)
		return
	}
	copy(globalBytes(pointer, len(data)), data)
	pGlobalUnlock.Call(handle)
	if r, _, _ := pSetClipboardData.Call(uintptr(format), handle); r == 0 {
		// On failure the memory is still ours.
		pGlobalFree.Call(handle)
	}
}

// globalBytes views locked global memory. It is the system's memory, not
// Go's, so the garbage collector neither moves nor frees it.
func globalBytes(pointer uintptr, n int) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(nil), pointer)), n)
}

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
