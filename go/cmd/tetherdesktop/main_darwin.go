package main

/*
#include "macos_darwin.h"
*/
import "C"

import (
	_ "embed"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/freeyoung/tether-core-go/core"
)

// The menu bar icon, a template image: scripts/make-icons.swift draws it.
//
//go:embed MenuBarIcon.png
var menuBarIcon []byte

// See core.LoadOrCreateIdentityFile.
const (
	identityFile     = "identity.p12"
	identityPassword = "tether"
)

func main() {
	home, _ := os.UserHomeDir()
	support := filepath.Join(home, "Library", "Application Support", "TetherDesktop")
	clipboard := pasteboard{}
	engine, err := core.NewEngine(core.Config{
		SupportDir: support, DownloadsDir: filepath.Join(home, "Downloads"), DeviceName: computerName(),
		IdentityFile: filepath.Join(support, identityFile), IdentityPassword: identityPassword,
		MarkDownloaded: quarantine, Log: logLine,
	}, clipboard, newDiscovery())
	if err != nil {
		logLine("cannot start: " + err.Error())
		systray.Run(func() {
			showError("Tether Desktop cannot start:\n\n" + err.Error())
			systray.Quit()
		}, nil)
		return
	}
	a := newApp(engine, clipboard)
	engine.Delegate = a
	current.Store(a)
	// Closes every session with a FIN; tetherd would keep a dead one. On
	// macOS, systray.Quit only stops the event loop, so Run returns without
	// calling onExit; a quit from outside (logout) calls onExit instead.
	stop := sync.OnceFunc(engine.Stop)

	systray.Run(func() {
		systray.SetTemplateIcon(menuBarIcon, menuBarIcon)
		systray.SetTooltip(title)
		C.td_init()
		go a.menuLoop()
		go a.checkUpdates()
		a.requestRefresh()
		engine.Start()
		go a.watchNetwork()
		// NSPasteboard posts no change notification; polling the change
		// count is cheap.
		go func() {
			for range time.Tick(500 * time.Millisecond) {
				engine.CheckLocalClipboard()
			}
		}()
		// `kill` and logout send SIGTERM. Quit normally, so that the
		// sessions are closed first.
		go func() {
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
			<-signals
			systray.Quit()
		}()
	}, stop)
	stop()
}
