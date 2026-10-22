//go:build windows || darwin

package main

import (
	"context"
	"fmt"
	"image/color"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/freeyoung/tether-core-go/core"
)

const title = "Tether Desktop"

// What each platform gives the shared code below (ui_windows.go,
// ui_darwin.go):
//
//	thisDevice                       "PC" or "Mac", as in "This PC"
//	menuDot(color) []byte            a status dot for a menu item
//	addPlatformItems(*app)           menu items only that platform has
//	showError(text)                  an error dialog
//	askPairing(ctx, text) answer     Allow, Deny or Later; ctx closes it
//	selectFiles() []string           a file chooser, for Send Files
//	chooseOne(text, names) (int, ok) a choice from a list
//	enterText(title, text, ok)       a text field
//	notify(heading, text)            a notification
//	notifyFile(heading, text, path)  one that shows the file when clicked
//	openURL(url)                     the default browser
//	startupEnabled(), setStartup(on) Open at Login
//	showAbout()                      the About window

type answer int

const (
	later answer = iota
	allow
	deny
)

// app is the tray icon, its menu and the dialogs. The engine calls it on its
// queue: nothing here may block that, so dialogs run on their own goroutines.
type app struct {
	engine    *core.Engine
	clipboard core.Clipboard

	refresh chan struct{}

	promptMu sync.Mutex
	prompts  []core.PairingRequest
	current  string
	cancel   context.CancelFunc

	greenDot, grayDot []byte

	updateMu sync.Mutex
	update   UpdateState
	latest   string
	outdated map[string]bool // peers we told the user about

	page messagesPageState
}

func newApp(engine *core.Engine, clipboard core.Clipboard) *app {
	return &app{engine: engine, clipboard: clipboard, refresh: make(chan struct{}, 1),
		greenDot: menuDot(color.NRGBA{52, 199, 89, 255}), grayDot: menuDot(color.NRGBA{128, 128, 128, 255})}
}

// MARK: - Menu

// menuLoop rebuilds the menu when the state changes, at most every 300 ms.
// A stopped engine never answers, so it waits for the state at most 5 s.
func (a *app) menuLoop() {
	for range a.refresh {
		time.Sleep(300 * time.Millisecond)
		if state, latest := a.updateState(); state == UpdateRequired {
			a.buildUpdateRequired(latest)
			continue
		}
		snap := make(chan core.Snapshot, 1)
		a.engine.Snapshot(func(s core.Snapshot) { snap <- s })
		select {
		case s := <-snap:
			a.build(s)
		case <-time.After(5 * time.Second):
		}
	}
}

func (a *app) requestRefresh() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

func onClick(item *systray.MenuItem, f func()) {
	go func() {
		for range item.ClickedCh {
			go f()
		}
	}()
}

func disabled(text string) *systray.MenuItem {
	item := systray.AddMenuItem(text, "")
	item.Disable()
	return item
}

func short(fp string) string { return fp[:8] + "…" + fp[len(fp)-8:] }

func (a *app) build(snap core.Snapshot) {
	systray.ResetMenu()
	status := "Tether Desktop " + appVersion + ": not listening"
	if snap.Listening {
		status = "Tether Desktop " + appVersion + ": listening"
	}
	disabled(status)
	if state, latest := a.updateState(); state == UpdateAvailable {
		onClick(systray.AddMenuItem("Update Available: Version "+latest+"…", ""), func() { openURL(releasesURL) })
	}
	onClick(systray.AddMenuItem("This "+thisDevice+": "+short(snap.Fingerprint)+"  (Copy)", ""), func() {
		a.clipboard.Write(core.TextContent(snap.Fingerprint))
	})
	for _, name := range snap.Outgoing {
		onClick(systray.AddMenuItem("Waiting for "+name+" to Approve — Cancel", ""), a.engine.CancelOutboundPairing)
	}

	if len(snap.Pending) > 0 {
		systray.AddSeparator()
		disabled("Pairing requests")
		for _, r := range snap.Pending {
			r := r
			onClick(systray.AddMenuItem(r.Name+"…", ""), func() { a.askToPair(r) })
		}
	}

	systray.AddSeparator()
	if len(snap.Peers) == 0 {
		disabled("No paired devices")
	} else {
		disabled("Paired devices")
	}
	for _, p := range snap.Peers {
		p := p
		state := "not connected"
		icon := a.grayDot
		if p.Connected {
			state, icon = "connected", a.greenDot
		}
		item := systray.AddMenuItem(fmt.Sprintf("%s — %s", p.Name, state), "")
		item.SetIcon(icon)
		send := item.AddSubMenuItem("Send Files…", "")
		if p.Connected {
			onClick(send, func() { a.chooseFiles(p.Fingerprint) })
		} else {
			send.SetTitle("Send Files… (Not Connected)")
			send.Disable()
		}
		if store := a.engine.Messages(); store != nil && store.Has(p.Fingerprint) {
			onClick(item.AddSubMenuItem("Messages…", ""), func() { a.openMessages(p.Fingerprint, p.Name, "") })
		}
		item.AddSubMenuItem(short(p.Fingerprint), "").Disable()
		onClick(item.AddSubMenuItem("Forget", ""), func() { a.engine.Forget(p.Fingerprint) })
	}
	for _, t := range snap.Sending {
		more := ""
		if t.Queued > 0 {
			more = fmt.Sprintf(", %d more waiting", t.Queued)
		}
		disabled(fmt.Sprintf("Sending %s to %s: %d%%%s", t.Filename, t.Peer, int(t.Fraction*100), more))
	}

	systray.AddSeparator()
	pair := systray.AddMenuItem("Pair with a Device", "")
	for _, d := range snap.Unpaired {
		d := d
		onClick(pair.AddSubMenuItem(d.Name, ""), func() { a.engine.PairWithDiscovered(d.Fingerprint) })
	}
	onClick(pair.AddSubMenuItem("Enter Address…", ""), a.pairManually)

	systray.AddSeparator()
	onClick(systray.AddMenuItemCheckbox("Open at Login", "", startupEnabled()), func() {
		a.toggle(func() error { return setStartup(!startupEnabled()) })
	})
	addPlatformItems(a)
	onClick(systray.AddMenuItem("About Tether Desktop…", ""), showAbout)
	onClick(systray.AddMenuItem("Quit Tether Desktop", ""), systray.Quit)
}

func (a *app) toggle(change func() error) {
	if err := change(); err != nil {
		showError(err.Error())
	}
	a.requestRefresh()
}

func (a *app) chooseFiles(fp string) {
	if files := selectFiles(); len(files) > 0 {
		a.engine.SendFiles(files, fp)
	}
}

// route sends files from Explorer's Send to or Finder's service: to the
// only connected device, or to the one the user picks.
func (a *app) route(files []string) {
	snap := make(chan core.Snapshot, 1)
	a.engine.Snapshot(func(s core.Snapshot) { snap <- s })
	var connected []core.PeerState
	for _, p := range (<-snap).Peers {
		if p.Connected {
			connected = append(connected, p)
		}
	}
	switch len(connected) {
	case 0:
		notify("No device is connected", "Open Tether on the phone, or wait for the device to connect.")
	case 1:
		a.engine.SendFiles(files, connected[0].Fingerprint)
	default:
		var names []string
		for _, p := range connected {
			names = append(names, p.Name)
		}
		what := fmt.Sprintf("%d files", len(files))
		if len(files) == 1 {
			what = filepath.Base(files[0])
		}
		if i, ok := chooseOne("Send "+what+" to:", names); ok {
			a.engine.SendFiles(files, connected[i].Fingerprint)
		}
	}
}

func (a *app) pairManually() {
	text, ok := enterText("Pair with a Device", "Enter the device's address, for example 192.168.1.20 or linux-pc.local.\n"+
		"The port is 5134 unless you add another, as host:port.", "Pair")
	if !ok {
		return
	}
	if host, port, ok := parseAddress(strings.TrimSpace(text)); ok {
		a.engine.PairWithComputer(host, port)
	}
}

// parseAddress takes "host", "host:port" or "[ipv6]:port".
func parseAddress(text string) (string, int, bool) {
	if text == "" {
		return "", 0, false
	}
	if host, port, err := net.SplitHostPort(text); err == nil {
		var p int
		if _, err := fmt.Sscanf(port, "%d", &p); err == nil && p > 0 && p < 65536 {
			return host, p, true
		}
	}
	return strings.Trim(text, "[]"), core.DefaultPort, true
}

// MARK: - Pairing prompts, one at a time

func (a *app) askToPair(r core.PairingRequest) {
	a.promptMu.Lock()
	defer a.promptMu.Unlock()
	if a.current == r.Fingerprint {
		return
	}
	for _, q := range a.prompts {
		if q.Fingerprint == r.Fingerprint {
			return
		}
	}
	a.prompts = append(a.prompts, r)
	if a.current == "" {
		go a.showPrompts()
	}
}

func (a *app) showPrompts() {
	for {
		a.promptMu.Lock()
		if len(a.prompts) == 0 {
			a.current, a.cancel = "", nil
			a.promptMu.Unlock()
			return
		}
		r := a.prompts[0]
		a.prompts = a.prompts[1:]
		ctx, cancel := context.WithCancel(context.Background())
		a.current, a.cancel = r.Fingerprint, cancel
		a.promptMu.Unlock()

		text := fmt.Sprintf("Pair with %s?\n\nA device asks to pair with this %s. Allow it only if you started the pairing "+
			"yourself, and the fingerprint matches the one on the device.\n\n%s",
			r.Name, thisDevice, core.FormatFingerprint(r.Fingerprint, 8))
		choice := askPairing(ctx, text)
		cancel()
		switch choice {
		case allow:
			a.engine.Approve(r.Fingerprint)
		case deny:
			a.engine.Reject(r.Fingerprint)
		}
	}
}

// MARK: - core.Delegate (the engine's queue: never block it)

func (a *app) PairingRequested(r core.PairingRequest) { a.askToPair(r) }

// PairingResolved: answered elsewhere, or lapsed: drop its prompt, and close
// it if it is open.
func (a *app) PairingResolved(fp string) {
	a.promptMu.Lock()
	kept := a.prompts[:0]
	for _, q := range a.prompts {
		if q.Fingerprint != fp {
			kept = append(kept, q)
		}
	}
	a.prompts = kept
	if a.current == fp && a.cancel != nil {
		a.cancel()
	}
	a.promptMu.Unlock()
	a.requestRefresh()
}

func (a *app) OutboundPairing(r core.OutboundResult) {
	switch r.Kind {
	case "waiting":
		if r.Peer != "" {
			go notify("Waiting for "+r.Peer, "Approve the request there, or run: tether accept "+a.engine.Identity.Fingerprint)
		}
	case "accepted":
		go notify("Paired with "+r.Peer, "The clipboard now syncs with it.")
	case "rejected":
		go notify("Pairing was rejected", "The other device did not accept this "+thisDevice+".")
	case "failed":
		go notify("Pairing failed", r.Reason)
	}
	a.requestRefresh()
}

func (a *app) OpenURL(url string) { go openURL(url) }

// FileReceived: clicking the notification shows the file.
func (a *app) FileReceived(path, peer string) {
	go notifyFile("File from "+peer, filepath.Base(path), path)
}

func (a *app) FileSent(name, peer string, ok bool, reason string) {
	if ok {
		go notify("Sent to "+peer, name)
	} else {
		go notify("Could not send "+name+" to "+peer, reason)
	}
	a.requestRefresh()
}

func (a *app) Failed(message string) { go showError(message) }

func (a *app) StateChanged() { a.requestRefresh() }

// PeerOutdated: tell the user once for each peer. version is the version of
// the peer's engine.
func (a *app) PeerOutdated(name, version string) {
	a.updateMu.Lock()
	told := a.outdated[name]
	if a.outdated == nil {
		a.outdated = map[string]bool{}
	}
	a.outdated[name] = true
	a.updateMu.Unlock()
	if !told {
		go notify(name+" needs an update", "Its engine, version "+version+", no longer works with this "+thisDevice+".")
	}
}

// NotificationPosted: a code in the text gets a Copy Code button.
func (a *app) NotificationPosted(n core.Notification) {
	go showPhoneNotification(n, core.FindCode(n.Text))
}

func (a *app) NotificationRemoved(fp, id string) { go removePhoneNotification(fp, id) }

// MARK: - Updates

// Each release publishes update.json (see release.yml). TETHER_UPDATE_URL
// points the check elsewhere, for tests.
const (
	projectURL  = "https://github.com/freeyoung/tether-desktop"
	updateURL   = projectURL + "/releases/latest/download/update.json"
	releasesURL = projectURL + "/releases/latest"
)

// checkUpdates looks for a new version soon after the start, then once a
// day.
func (a *app) checkUpdates() {
	url, ok := updateSource(appVersion, os.Getenv("TETHER_UPDATE_URL"))
	if !ok {
		return
	}
	time.Sleep(10 * time.Second)
	for {
		if info, err := FetchUpdateInfo(context.Background(), url); err != nil {
			logLine(err.Error())
		} else {
			a.applyUpdate(info)
		}
		time.Sleep(24 * time.Hour)
	}
}

// updateSource tells where to look for updates: TETHER_UPDATE_URL when it is
// set, else update.json of the latest release. A build without a release
// version does not look.
func updateSource(version, override string) (string, bool) {
	if override != "" {
		return override, true
	}
	if strings.HasSuffix(version, "-dev") {
		return "", false
	}
	return updateURL, true
}

func (a *app) applyUpdate(info UpdateInfo) {
	state := info.State(appVersion)
	a.updateMu.Lock()
	before := a.update
	a.update, a.latest = state, info.Latest
	a.updateMu.Unlock()
	if state == before {
		return
	}
	if state == UpdateRequired {
		// This version no longer works with the others: stop syncing
		// until the user installs the update.
		logLine(fmt.Sprintf("version %s is required, this is %s: stopping", info.MinSupported, appVersion))
		a.engine.Stop()
		go func() {
			showError(fmt.Sprintf("Tether Desktop %s or later is required. This version no longer works with "+
				"the others, so it stopped syncing.\n\nThe download page opens now.", info.MinSupported))
			openURL(releasesURL)
		}()
	}
	a.requestRefresh()
}

func (a *app) updateState() (UpdateState, string) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	return a.update, a.latest
}

// buildUpdateRequired is the whole menu once the engine stopped for an
// update.
func (a *app) buildUpdateRequired(latest string) {
	systray.ResetMenu()
	disabled("Tether Desktop " + appVersion + ": update required")
	onClick(systray.AddMenuItem("Download Version "+latest+"…", ""), func() { openURL(releasesURL) })
	systray.AddSeparator()
	onClick(systray.AddMenuItem("Quit Tether Desktop", ""), systray.Quit)
}

// MARK: - Network

// watchNetwork reconnects to computers after a sleep or a network change:
// sessions with them do not survive either.
func (a *app) watchNetwork() {
	last := interfacesKey()
	tick := time.Now()
	for range time.Tick(5 * time.Second) {
		now := time.Now()
		// A tick much later than planned means the computer slept.
		slept := now.Round(0).Sub(tick.Round(0)) > 30*time.Second
		tick = now
		key := interfacesKey()
		if slept || key != last {
			last = key
			a.engine.NetworkDidChange()
		}
	}
}

func interfacesKey() string {
	var parts []string
	interfaces, _ := net.Interfaces()
	for _, ifi := range interfaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, addr := range addrs {
			parts = append(parts, ifi.Name+"="+addr.String())
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
