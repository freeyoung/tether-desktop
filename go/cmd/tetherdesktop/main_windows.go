package main

import (
	_ "embed"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/Microsoft/go-winio"
	"github.com/ncruces/zenity"
	"golang.org/x/sys/windows"

	"tetherdesktop/core"
)

//go:embed AppIcon.ico
var appIcon []byte

func pipeName() string {
	name := "user"
	if u, err := user.Current(); err == nil {
		name = filepath.Base(u.Username)
	}
	return `\\.\pipe\TetherDesktop-` + name
}

// One instance per user: a second start (from Explorer's Send to, with
// files) hands its files to the first and exits.
func main() {
	mutexName, _ := windows.UTF16PtrFromString(`Local\TetherDesktop`)
	mutex, err := windows.CreateMutex(nil, false, mutexName)
	if err == windows.ERROR_ALREADY_EXISTS {
		forward(os.Args[1:])
		return
	}
	defer windows.CloseHandle(mutex)

	var files []string
	for _, arg := range os.Args[1:] {
		if abs, err := filepath.Abs(arg); err == nil {
			files = append(files, abs)
		}
	}

	ui, err := startUIThread()
	if err != nil {
		fatal(err)
	}
	clipboard := newWindowsClipboard(ui)
	host, _ := os.Hostname()
	engine, err := core.NewEngine(core.Config{
		SupportDir: supportDir(), DownloadsDir: downloadsDir(), DeviceName: host,
		Protector: dpapi{}, MarkDownloaded: markDownloaded, Log: logLine,
	}, clipboard, newDiscovery())
	if err != nil {
		fatal(err)
	}
	a := newApp(engine, clipboard)
	engine.Delegate = a
	// WM_CLIPBOARDUPDATE tells of every change; no polling is needed.
	check := engine.CheckLocalClipboard
	ui.onClipboardChange.Store(&check)

	registerAppID()
	systray.Run(func() {
		systray.SetIcon(appIcon)
		systray.SetTooltip(title)
		go a.menuLoop()
		go a.checkUpdates()
		a.requestRefresh()
		refreshStartup()
		refreshSendTo()
		engine.Start()
		go serve(a)
		go a.watchNetwork()
		if len(files) > 0 {
			// Started from Send to with files: give paired computers a moment to connect.
			go func() {
				time.Sleep(3 * time.Second)
				a.route(files)
			}()
		}
	}, func() {
		// Closes every session with a FIN; tetherd would keep a dead one.
		engine.Stop()
	})
}

func fatal(err error) {
	logLine("cannot start: " + err.Error())
	_ = zenity.Error("Tether Desktop cannot start:\n\n"+err.Error(), zenity.Title(title))
	os.Exit(1)
}

func forward(args []string) {
	if len(args) == 0 {
		return
	}
	var files []string
	for _, arg := range args {
		if abs, err := filepath.Abs(arg); err == nil {
			files = append(files, abs)
		}
	}
	timeout := 3 * time.Second
	conn, err := winio.DialPipe(pipeName(), &timeout)
	if err != nil {
		logLine("could not hand files to the running app: " + err.Error())
		return
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, strings.Join(files, "\n"))
}

func serve(a *app) {
	l, err := winio.ListenPipe(pipeName(), nil)
	if err != nil {
		logLine("pipe: " + err.Error())
		return
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			logLine("pipe: " + err.Error())
			return
		}
		data, _ := io.ReadAll(conn)
		conn.Close()
		if files := strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' || r == '\r' }); len(files) > 0 {
			go a.route(files)
		}
	}
}
