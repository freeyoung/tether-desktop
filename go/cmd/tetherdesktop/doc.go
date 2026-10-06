// tetherdesktop is the tray app of Tether Desktop, in Go, for Windows and
// macOS: the engine in package core and a tray or menu bar menu. tray.go is
// shared; the clipboard, DNS-SD, dialogs and notifications are per platform
// (*_windows.go, and *_darwin.go with macos_darwin.m).
//
//	GOOS=windows GOARCH=amd64 go build -ldflags "-s -w -H windowsgui" ./cmd/tetherdesktop
//	scripts/build-macos.sh   (macOS: the .app bundle)
package main
