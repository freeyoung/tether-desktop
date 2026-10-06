package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The identity, the pairings and the log.
func supportDir() string {
	return filepath.Join(os.Getenv("APPDATA"), "TetherDesktop")
}

func logFile() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "TetherDesktop", "tetherdesktop.log")
}

func executable() string {
	path, _ := os.Executable()
	return path
}

var logMu sync.Mutex

// logLine appends to a small log file. It never holds clipboard or file content.
func logLine(line string) {
	logMu.Lock()
	defer logMu.Unlock()
	path := logFile()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if info, err := os.Stat(path); err == nil && info.Size() > 1<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\r\n", time.Now().Format("2006-01-02 15:04:05"), line)
}

func downloadsDir() string {
	path, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Downloads")
	}
	return path
}

// dpapi encrypts the identity file for the current Windows user.
type dpapi struct{}

var entropy = []byte("TetherDesktop identity")

func blob(data []byte) *windows.DataBlob {
	if len(data) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func (dpapi) Protect(data []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptProtectData(blob(data), nil, blob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func (dpapi) Unprotect(data []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(data), nil, blob(entropy), 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// markDownloaded writes a Mark of the Web, so SmartScreen and Office check a
// received file before it runs, as they do for a browser download.
func markDownloaded(path string) {
	if err := os.WriteFile(path+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
		logLine("could not mark a received file: " + err.Error())
	}
}

// Open at Login: an entry in HKCU\...\Run.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runName = "TetherDesktop"

func commandLine() string { return `"` + executable() + `"` }

func startupEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	value, _, err := k.GetStringValue(runName)
	return err == nil && strings.EqualFold(value, commandLine())
}

func setStartup(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if on {
		return k.SetStringValue(runName, commandLine())
	}
	if err := k.DeleteValue(runName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// refreshStartup points an entry from another version or folder at this exe.
func refreshStartup() {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return
	}
	value, _, err := k.GetStringValue(runName)
	k.Close()
	if err == nil && !strings.EqualFold(value, commandLine()) {
		if err := setStartup(true); err != nil {
			logLine("could not update the login entry: " + err.Error())
		}
	}
}

// Send to: a shortcut to this exe, which Explorer starts with the files.
func sendToShortcut() string {
	path, err := windows.KnownFolderPath(windows.FOLDERID_SendTo, 0)
	if err != nil {
		path = filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\SendTo`)
	}
	return filepath.Join(path, "Tether Desktop.lnk")
}

func sendToEnabled() bool {
	_, err := os.Stat(sendToShortcut())
	return err == nil
}

func setSendTo(on bool) error {
	if !on {
		return os.Remove(sendToShortcut())
	}
	runtimeLockOLE.Lock()
	defer runtimeLockOLE.Unlock()
	// COM belongs to a thread: stay on this one until done.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		// Already initialized on this thread is fine.
		if oleErr, ok := err.(*ole.OleError); !ok || oleErr.Code() != 1 {
			return err
		}
	}
	defer ole.CoUninitialize()
	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return err
	}
	defer unknown.Release()
	shell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer shell.Release()
	link, err := oleutil.CallMethod(shell, "CreateShortcut", sendToShortcut())
	if err != nil {
		return err
	}
	shortcut := link.ToIDispatch()
	defer shortcut.Release()
	for name, value := range map[string]string{
		"TargetPath":   executable(),
		"Description":  "Send to a device with Tether Desktop",
		"IconLocation": executable() + ",0",
	} {
		if _, err := oleutil.PutProperty(shortcut, name, value); err != nil {
			return err
		}
	}
	_, err = oleutil.CallMethod(shortcut, "Save")
	return err
}

var runtimeLockOLE sync.Mutex

func refreshSendTo() {
	if sendToEnabled() {
		if err := setSendTo(true); err != nil {
			logLine("could not update the Send to shortcut: " + err.Error())
		}
	}
}

func openURL(url string) {
	verb, _ := windows.UTF16PtrFromString("open")
	target, _ := windows.UTF16PtrFromString(url)
	_ = windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL)
}
