package main

import (
	_ "embed"
	"encoding/base64"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed winres/icon.png
var iconPNG []byte

// appID names this app to Windows. A toast from an unregistered ID shows
// no icon and no app name.
const appID = "TetherDesktop"

// registerAppID gives the toasts this app's name and icon: Windows reads
// both from HKCU\Software\Classes\AppUserModelId\<ID> for an unpackaged app.
func registerAppID() {
	icon := filepath.Join(os.Getenv("LOCALAPPDATA"), "TetherDesktop", "AppIcon.png")
	_ = os.MkdirAll(filepath.Dir(icon), 0o755)
	if err := os.WriteFile(icon, iconPNG, 0o644); err != nil {
		logLine("could not write the notification icon: " + err.Error())
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+appID, registry.SET_VALUE)
	if err != nil {
		logLine("could not register the app for notifications: " + err.Error())
		return
	}
	defer k.Close()
	_ = k.SetStringValue("DisplayName", title)
	_ = k.SetStringValue("IconUri", icon)
	id, _ := windows.UTF16PtrFromString(appID)
	windows.NewLazySystemDLL("shell32.dll").NewProc("SetCurrentProcessExplicitAppUserModelID").Call(unsafePointer(id))
}

// The title, text and click target come in environment variables and are
// escaped there, so a file name can never become part of the script.
const toastScript = `
$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$heading = [Security.SecurityElement]::Escape($env:TETHER_TOAST_TITLE)
$text = [Security.SecurityElement]::Escape($env:TETHER_TOAST_TEXT)
$launch = [Security.SecurityElement]::Escape($env:TETHER_TOAST_LAUNCH)
$activation = ''
if ($launch) { $activation = " activationType=""protocol"" launch=""$launch""" }
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml("<toast$activation><visual><binding template=""ToastGeneric""><text>$heading</text><text>$text</text></binding></visual></toast>")
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($env:TETHER_TOAST_APPID).Show([Windows.UI.Notifications.ToastNotification]::new($xml))
`

func notify(heading, text string) { notifyOpening(heading, text, "") }

// notifyOpening shows a toast; clicking it opens target (a folder), if any.
func notifyOpening(heading, text, target string) {
	launch := ""
	if target != "" {
		launch = (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(target)}).String()
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeCommand(toastScript))
	cmd.Env = append(os.Environ(), "TETHER_TOAST_TITLE="+heading, "TETHER_TOAST_TEXT="+text,
		"TETHER_TOAST_LAUNCH="+launch, "TETHER_TOAST_APPID="+appID)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		logLine("could not show a notification: " + err.Error() + " " + strings.TrimSpace(string(out)))
	}
}

// encodeCommand is what powershell -EncodedCommand takes: UTF-16LE in base64.
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		b[2*i], b[2*i+1] = byte(u), byte(u>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func unsafePointer(p *uint16) uintptr { return uintptr(unsafe.Pointer(p)) }
