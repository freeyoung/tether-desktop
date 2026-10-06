package main

import (
	"errors"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	pRegisterClassEx            = user32.NewProc("RegisterClassExW")
	pCreateWindowEx             = user32.NewProc("CreateWindowExW")
	pDefWindowProc              = user32.NewProc("DefWindowProcW")
	pGetMessage                 = user32.NewProc("GetMessageW")
	pTranslateMessage           = user32.NewProc("TranslateMessage")
	pDispatchMessage            = user32.NewProc("DispatchMessageW")
	pPostMessage                = user32.NewProc("PostMessageW")
	pAddClipboardFormatListener = user32.NewProc("AddClipboardFormatListener")
	pOpenClipboard              = user32.NewProc("OpenClipboard")
	pCloseClipboard             = user32.NewProc("CloseClipboard")
	pEmptyClipboard             = user32.NewProc("EmptyClipboard")
	pEnumClipboardFormats       = user32.NewProc("EnumClipboardFormats")
	pGetClipboardFormatName     = user32.NewProc("GetClipboardFormatNameW")
	pGetClipboardData           = user32.NewProc("GetClipboardData")
	pSetClipboardData           = user32.NewProc("SetClipboardData")
	pRegisterClipboardFormat    = user32.NewProc("RegisterClipboardFormatW")
	pGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	pGlobalAlloc                = kernel32.NewProc("GlobalAlloc")
	pGlobalLock                 = kernel32.NewProc("GlobalLock")
	pGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
	pGlobalSize                 = kernel32.NewProc("GlobalSize")
	pGlobalFree                 = kernel32.NewProc("GlobalFree")
)

const (
	wmClipboardUpdate = 0x031D
	wmCall            = 0x8000 + 1  // WM_APP + 1
	hwndMessage       = ^uintptr(2) // HWND_MESSAGE = -3
	gmemMoveable      = 0x0002
)

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

// uiThread is one OS thread with a message-only window. It receives
// WM_CLIPBOARDUPDATE, and runs every clipboard call: writing the clipboard
// needs a window of this thread as its owner.
type uiThread struct {
	hwnd  uintptr
	calls chan func()
	// Set once the engine exists; WM_CLIPBOARDUPDATE calls it.
	onClipboardChange atomic.Pointer[func()]
}

func startUIThread() (*uiThread, error) {
	t := &uiThread{calls: make(chan func(), 64)}
	ready := make(chan error)
	go func() {
		runtime.LockOSThread()
		className, _ := windows.UTF16PtrFromString("TetherDesktopClipboard")
		instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
		proc := windows.NewCallback(func(hwnd, message, wParam, lParam uintptr) uintptr {
			switch message {
			case wmClipboardUpdate:
				if f := t.onClipboardChange.Load(); f != nil {
					(*f)()
				}
				return 0
			case wmCall:
				for {
					select {
					case f := <-t.calls:
						f()
					default:
						return 0
					}
				}
			}
			r, _, _ := pDefWindowProc.Call(hwnd, message, wParam, lParam)
			return r
		})
		class := wndClassEx{wndProc: proc, instance: windows.Handle(instance), className: className}
		class.size = uint32(unsafe.Sizeof(class))
		if r, _, err := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); r == 0 {
			ready <- err
			return
		}
		hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, instance, 0)
		if hwnd == 0 {
			ready <- err
			return
		}
		t.hwnd = hwnd
		if r, _, err := pAddClipboardFormatListener.Call(hwnd); r == 0 {
			logLine("could not listen to the clipboard: " + err.Error())
		}
		ready <- nil
		var m msg
		for {
			r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	if err := <-ready; err != nil {
		return nil, errors.New("could not create the clipboard window: " + err.Error())
	}
	return t, nil
}

// do runs f on the UI thread and waits for it.
func (t *uiThread) do(f func()) {
	done := make(chan struct{})
	t.calls <- func() {
		defer close(done)
		f()
	}
	pPostMessage.Call(t.hwnd, wmCall, 0, 0)
	<-done
}
