package main

import (
	"encoding/binary"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"tetherdesktop/core"
)

// The About window is a task dialog: unlike a message box, it shows the
// app's icon and a link that opens in the browser. It needs Common Controls
// 6, which the exe's manifest asks for (winres/winres.json).

var (
	comctl32             = windows.NewLazySystemDLL("comctl32.dll")
	pTaskDialogIndirect  = comctl32.NewProc("TaskDialogIndirect")
	pLoadImage           = user32.NewProc("LoadImageW")
	pDestroyIcon         = user32.NewProc("DestroyIcon")
	pGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	aboutCallback        = syscall.NewCallback(aboutEvent)
	tdnHyperlinkClicked  = uintptr(3)
	tdfEnableHyperlinks  = uint32(0x0001)
	tdfUseHiconMain      = uint32(0x0002)
	tdfAllowCancellation = uint32(0x0008)
	tdcbfCloseButton     = uint32(0x0020)
)

// aboutEvent opens a clicked link in the default browser.
func aboutEvent(hwnd, msg, wParam, lParam, data uintptr) uintptr {
	if msg == tdnHyperlinkClicked && lParam != 0 {
		// lParam is the link's address, which Windows owns.
		openURL(windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&lParam))))
	}
	return 0
}

func showAbout() {
	// The dialog's window belongs to this thread until it closes.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	instance, _, _ := pGetModuleHandle.Call(0)
	iconName, _ := windows.UTF16PtrFromString("APP") // RT_GROUP_ICON in winres.json
	const imageIcon, lrDefaultSize = 1, 0x40
	icon, _, _ := pLoadImage.Call(instance, uintptr(unsafe.Pointer(iconName)), imageIcon, 0, 0, lrDefaultSize)

	title, _ := windows.UTF16PtrFromString("About Tether Desktop")
	heading, _ := windows.UTF16PtrFromString("Tether Desktop " + core.Version)
	content, _ := windows.UTF16PtrFromString("A desktop peer for Tether: the clipboard and files, shared between " +
		"your iPhone and your computers.\n\n<a href=\"" + projectURL + "\">" + projectURL + "</a>")
	footer, _ := windows.UTF16PtrFromString("© 2026 Eric Qian. MIT License.")

	// TASKDIALOGCONFIG is packed to 1 byte in commctrl.h, so it is written
	// field by field at the offsets of its 64-bit layout.
	var config [160]byte
	put32 := func(offset int, v uint32) { binary.LittleEndian.PutUint32(config[offset:], v) }
	put64 := func(offset int, v uintptr) { binary.LittleEndian.PutUint64(config[offset:], uint64(v)) }
	flags := tdfEnableHyperlinks | tdfAllowCancellation
	if icon != 0 {
		flags |= tdfUseHiconMain
	}
	put32(0, uint32(len(config)))               // cbSize
	put64(12, instance)                         // hInstance
	put32(20, flags)                            // dwFlags
	put32(24, tdcbfCloseButton)                 // dwCommonButtons
	put64(28, uintptr(unsafe.Pointer(title)))   // pszWindowTitle
	put64(36, icon)                             // hMainIcon
	put64(44, uintptr(unsafe.Pointer(heading))) // pszMainInstruction
	put64(52, uintptr(unsafe.Pointer(content))) // pszContent
	put64(132, uintptr(unsafe.Pointer(footer))) // pszFooter
	put64(140, aboutCallback)                   // pfCallback
	pTaskDialogIndirect.Call(uintptr(unsafe.Pointer(&config[0])), 0, 0, 0)
	runtime.KeepAlive(title)
	runtime.KeepAlive(heading)
	runtime.KeepAlive(content)
	runtime.KeepAlive(footer)
	if icon != 0 {
		pDestroyIcon.Call(icon)
	}
}
