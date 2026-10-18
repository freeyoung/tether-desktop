package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/freeyoung/tether-core-go/core"
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
