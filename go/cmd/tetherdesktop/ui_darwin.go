package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit -framework UserNotifications -framework ServiceManagement -framework SystemConfiguration
#include <stdlib.h>
#include "macos_darwin.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/freeyoung/tether-core-go/core"
)

// The macOS side of tray.go, on top of macos_darwin.m: AppKit dialogs,
// UNUserNotificationCenter, SMAppService, NSPasteboard and DNS-SD.

const thisDevice = "Mac"

// Finder's service menu needs no item here; it comes from Info.plist.
func addPlatformItems(*app) {}

// goString turns a malloc'ed C string into Go and frees it.
func goString(s *C.char) (string, bool) {
	if s == nil {
		return "", false
	}
	defer C.free(unsafe.Pointer(s))
	return C.GoString(s), true
}

func withC(s string, f func(*C.char)) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	f(cs)
}

func logLine(line string) { withC(line, func(s *C.char) { C.td_log(s) }) }

func computerName() string {
	name, _ := goString(C.td_computer_name())
	return name
}

// MARK: - Dialogs and notifications

func showError(text string) { withC(text, func(s *C.char) { C.td_show_error(s) }) }

// showAbout opens the standard About panel: icon, name and version from
// Info.plist, and a link to the project.
func showAbout() { withC(projectURL, func(s *C.char) { C.td_show_about(s) }) }

var pairingTokens atomic.Int64

func askPairing(ctx context.Context, text string) answer {
	token := C.int64_t(pairingTokens.Add(1))
	stop := context.AfterFunc(ctx, func() { C.td_cancel_pairing(token) })
	defer stop()
	var choice C.int
	withC(text, func(s *C.char) { choice = C.td_ask_pairing(s, token) })
	switch choice {
	case 1:
		return allow
	case 2:
		return deny
	}
	return later
}

func selectFiles() []string {
	paths, ok := goString(C.td_select_files())
	if !ok {
		return nil
	}
	return strings.Split(paths, "\n")
}

func chooseOne(text string, names []string) (int, bool) {
	var choice C.int
	withC(text, func(t *C.char) {
		withC(strings.Join(names, "\n"), func(n *C.char) {
			withC("Send", func(ok *C.char) { choice = C.td_choose_one(t, n, ok) })
		})
	})
	return int(choice), choice >= 0
}

func enterText(heading, text, ok string) (string, bool) {
	var out *C.char
	withC(heading, func(h *C.char) {
		withC(text, func(t *C.char) {
			withC(ok, func(o *C.char) { out = C.td_enter_text(h, t, o) })
		})
	})
	return goString(out)
}

func notify(heading, text string) { notifyFile(heading, text, "") }

// notifyFile: clicking the notification shows the file in Finder.
func notifyFile(heading, text, path string) {
	withC(heading, func(h *C.char) {
		withC(text, func(t *C.char) {
			withC(path, func(p *C.char) { C.td_notify(h, t, p) })
		})
	})
}

// showPhoneNotification shows a notification of an Android phone, grouped by
// its app. A later one with the same ID replaces it. A click calls
// tdPhoneNotificationClicked; code, if not "", gets a Copy Code button.
func showPhoneNotification(n core.Notification, code string) {
	title, subtitle := n.AppName, n.Title
	if title == "" {
		title = n.PeerName
	}
	withC(phoneNotificationID(n.PeerFingerprint, n.ID), func(id *C.char) {
		withC(n.PeerFingerprint+" "+n.App, func(thread *C.char) {
			withC(title, func(t *C.char) {
				withC(subtitle, func(st *C.char) {
					withC(n.Text, func(b *C.char) {
						withC(n.PeerFingerprint, func(peer *C.char) {
							withC(n.App, func(app *C.char) {
								withC(code, func(c *C.char) { C.td_phone_notify(id, thread, t, st, b, peer, app, c) })
							})
						})
					})
				})
			})
		})
	})
}

func showMessagesWindow(page, title string) {
	withC(page, func(p *C.char) {
		withC(title, func(t *C.char) { C.td_messages_show(p, t) })
	})
}

func evalInMessages(js string) { withC(js, func(s *C.char) { C.td_messages_eval(s) }) }

func removePhoneNotification(fp, id string) {
	withC(phoneNotificationID(fp, id), func(s *C.char) { C.td_phone_unnotify(s) })
}

func phoneNotificationID(fp, id string) string { return "phone " + fp + " " + id }

func openURL(url string) { withC(url, func(s *C.char) { C.td_open_url(s) }) }

func quarantine(path string) { withC(path, func(s *C.char) { C.td_quarantine(s) }) }

func startupEnabled() bool { return C.td_login_enabled() != 0 }

func setStartup(on bool) error {
	flag := C.int(0)
	if on {
		flag = 1
	}
	if message, failed := goString(C.td_set_login(flag)); failed {
		return errors.New(message)
	}
	return nil
}

// menuDot is a filled circle for the device list: 32 px, shown at 16 pt.
func menuDot(c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			if d := dx*dx + dy*dy; d <= 64 {
				img.SetNRGBA(x, y, c)
			} else if d <= 76 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, 110})
			}
		}
	}
	var out bytes.Buffer
	_ = png.Encode(&out, img)
	return out.Bytes()
}

// MARK: - Callbacks from macos_darwin.m

// The app that the callbacks below go to.
var current atomic.Pointer[app]

//export tdServiceFiles
func tdServiceFiles(paths *C.char) {
	files := strings.Split(C.GoString(paths), "\n")
	if a := current.Load(); a != nil {
		go a.route(files)
	}
}

// tdWebMessage: a request of the Messages page, on the main thread.
//
//export tdWebMessage
func tdWebMessage(message *C.char) {
	raw := C.GoString(message)
	if a := current.Load(); a != nil {
		go a.handlePage(raw)
	}
}

// tdPhoneNotificationClicked: who is the title of the notification, the
// sender in a messages app.
//
//export tdPhoneNotificationClicked
func tdPhoneNotificationClicked(peer, who, app *C.char) {
	p, w, ap := C.GoString(peer), C.GoString(who), C.GoString(app)
	if a := current.Load(); a != nil {
		go a.openFromNotification(p, w, ap)
	}
}

//export tdCopyCode
func tdCopyCode(code *C.char) {
	text := C.GoString(code)
	if a := current.Load(); a != nil {
		go a.clipboard.Write(core.TextContent(text))
	}
}

//export tdDidWake
func tdDidWake() {
	if a := current.Load(); a != nil {
		go a.engine.NetworkDidChange()
	}
}

// MARK: - The pasteboard

type pasteboard struct{}

func (pasteboard) ChangeCount() uint64 { return uint64(C.td_change_count()) }

// Read skips secrets and Finder file copies, and picks text or image by the
// order the owner wrote them in: see core.ChooseMac.
func (pasteboard) Read() (core.Content, bool) {
	types, _ := goString(C.td_pasteboard_types())
	var length C.int64_t
	switch core.ChooseMac(strings.Split(types, "\n")) {
	case core.ChooseText:
		if data := C.td_pasteboard_text(&length); data != nil {
			defer C.free(unsafe.Pointer(data))
			return core.TextContent(C.GoStringN(data, C.int(length))), true
		}
	case core.ChooseImage:
		if data := C.td_pasteboard_png(&length); data != nil {
			defer C.free(unsafe.Pointer(data))
			return core.PNGContent(C.GoBytes(unsafe.Pointer(data), C.int(length))), true
		}
	}
	return core.Content{}, false
}

func (pasteboard) Write(c core.Content) uint64 {
	data := []byte(c.Text)
	if c.IsPNG {
		data = c.PNG
	}
	p := C.CBytes(data)
	defer C.free(p)
	if c.IsPNG {
		return uint64(C.td_write_png((*C.char)(p), C.int64_t(len(data))))
	}
	return uint64(C.td_write_text((*C.char)(p), C.int64_t(len(data))))
}

// MARK: - DNS-SD

// discovery advertises and browses through mDNSResponder. On macOS only it
// can send multicast; a socket of the app's own gets EHOSTUNREACH.
type discovery struct {
	mu        sync.Mutex
	onResults func([]core.Discovered)
	// Per service name: the interfaces it is seen on, and where it is.
	seen     map[string]map[uint32]bool
	resolved map[string]core.Discovered
	stop     chan struct{}
	done     chan struct{}
}

var browsing atomic.Pointer[discovery]

func newDiscovery() *discovery { return &discovery{} }

func (d *discovery) Start(instance string, port int, fingerprint string, onResults func([]core.Discovered)) {
	d.Stop()
	d.mu.Lock()
	d.onResults = onResults
	d.seen = map[string]map[uint32]bool{}
	d.resolved = map[string]core.Discovered{}
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	stop, done := d.stop, d.done
	d.mu.Unlock()
	browsing.Store(d)
	go d.run(instance, port, fingerprint, stop, done)
}

// run owns the connection to mDNSResponder: only this goroutine uses it.
func (d *discovery) run(instance string, port int, fingerprint string, stop, done chan struct{}) {
	defer close(done)
	var fd C.int
	withC(instance, func(n *C.char) {
		withC(fingerprint, func(f *C.char) { fd = C.td_dnssd_start(n, C.int(port), f) })
	})
	if fd < 0 {
		logLine("dns-sd: could not start")
		return
	}
	defer C.td_dnssd_stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 500)
		if err != nil && !errors.Is(err, unix.EINTR) {
			logLine("dns-sd: " + err.Error())
			return
		}
		if n > 0 && C.td_dnssd_process() != 0 {
			logLine("dns-sd: the connection to mDNSResponder failed")
			return
		}
	}
}

func (d *discovery) Stop() {
	d.mu.Lock()
	stop, done := d.stop, d.done
	d.stop, d.done = nil, nil
	d.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (d *discovery) report() {
	list := make([]core.Discovered, 0, len(d.resolved))
	for _, r := range d.resolved {
		list = append(list, r)
	}
	if d.onResults != nil {
		d.onResults(list)
	}
}

//export tdBrowseAdded
func tdBrowseAdded(name *C.char, iface C.uint32_t) {
	d := browsing.Load()
	if d == nil {
		return
	}
	n := C.GoString(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[n] == nil {
		d.seen[n] = map[uint32]bool{}
	}
	d.seen[n][uint32(iface)] = true
}

//export tdBrowseRemoved
func tdBrowseRemoved(name *C.char, iface C.uint32_t) {
	d := browsing.Load()
	if d == nil {
		return
	}
	n := C.GoString(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen[n], uint32(iface))
	if len(d.seen[n]) == 0 {
		delete(d.seen, n)
		if _, ok := d.resolved[n]; ok {
			delete(d.resolved, n)
			d.report()
		}
	}
}

// tdResolved: a service and 1 of its IPv4 addresses. A service is seen once
// per interface, and a computer can have several addresses: keep them all.
// Link-local addresses come last.
//
//export tdResolved
func tdResolved(name, fingerprint, ip *C.char, port, computer C.int) {
	d := browsing.Load()
	if d == nil {
		return
	}
	n, host := C.GoString(name), C.GoString(ip)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[n] == nil {
		return
	}
	found := d.resolved[n]
	if found.Fingerprint != C.GoString(fingerprint) || found.Port != int(port) {
		found = core.Discovered{Name: n, Fingerprint: C.GoString(fingerprint), Port: int(port), IsComputer: computer != 0}
	} else if slices.Contains(found.Hosts, host) {
		return
	}
	found.Hosts = append(slices.Clone(found.Hosts), host)
	linkLocal := func(h string) int {
		if strings.HasPrefix(h, "169.254.") {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(found.Hosts, func(a, b string) int { return linkLocal(a) - linkLocal(b) })
	d.resolved[n] = found
	d.report()
}
