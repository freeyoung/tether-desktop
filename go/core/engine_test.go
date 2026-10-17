package core

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The engine against a client that behaves like the iPhone app.

func phoneSetup(t *testing.T) (*testNode, *fakePhone) {
	n := newNode(t, "Test PC")
	p := newPhone(t)
	p.connect(n.port, true)
	return n, p
}

func TestFingerprintMatches(t *testing.T) {
	n, p := phoneSetup(t)
	if p.serverFingerprint() != n.engine.Identity.Fingerprint {
		t.Fatal("fingerprint differs")
	}
}

func TestUnpairedPhoneIsRefused(t *testing.T) {
	n, p := phoneSetup(t)
	p.send(ClipboardSet("secret"))
	if m := p.expect("error"); m.Message != "unauthorized" {
		t.Fatal(m.Message)
	}
	if n.clipboard.writeCount() != 0 || n.delegate.requestCount() != 0 {
		t.Fatal("refused peer changed something")
	}
}

func TestPairingNeedsApproval(t *testing.T) {
	n, p := phoneSetup(t)
	p.send(PairRequest("Test iPhone"))
	if m := p.expect("pair_pending"); m.DeviceName != "Test PC" {
		t.Fatal(m.DeviceName)
	}
	if !eventually(5*time.Second, func() bool { return n.delegate.requestCount() == 1 }) || n.delegate.firstRequest().Name != "Test iPhone" {
		t.Fatal("no prompt")
	}
	time.Sleep(300 * time.Millisecond)
	if len(p.received("pair_accepted")) != 0 {
		t.Fatal("accepted without approval")
	}
	p.send(ClipboardSet("too early"))
	p.expect("error")
	n.engine.Approve(p.identity.Fingerprint)
	p.expect("pair_accepted")
}

func TestRejectedPhoneIsDropped(t *testing.T) {
	n, p := phoneSetup(t)
	p.send(PairRequest("Test iPhone"))
	p.expect("pair_pending")
	eventually(5*time.Second, func() bool { return n.delegate.requestCount() == 1 })
	n.engine.Reject(p.identity.Fingerprint)
	if !eventually(5*time.Second, func() bool { return !p.ready.Load() }) {
		t.Fatal("not dropped")
	}
	p.connect(n.port, false)
	time.Sleep(500 * time.Millisecond)
	if p.ready.Load() {
		t.Fatal("a rejected phone is dropped for the rest of the run")
	}
}

func TestHelloIsAnsweredOnceAndAnnouncesImages(t *testing.T) {
	n, p := phoneSetup(t)
	p.send(PairRequest("Test iPhone"))
	p.expect("pair_pending")
	eventually(5*time.Second, func() bool { return n.delegate.requestCount() == 1 })
	n.engine.Approve(p.identity.Fingerprint)
	p.expect("pair_accepted")
	p.send(Hello("clipboard_image"))
	if m := p.expect("hello"); !slices.Equal(m.Features, []string{"clipboard_image", "tether-desktop/" + Version}) {
		t.Fatal(m.Features)
	}
	p.send(Hello("clipboard_image"))
	time.Sleep(300 * time.Millisecond)
	if len(p.received("hello")) != 0 {
		t.Fatal("hello answered twice")
	}
}

func TestClipboardTextBothWaysWithoutEcho(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	p.send(ClipboardSet("从手机来"))
	if !eventually(5*time.Second, func() bool { return n.clipboard.holds(TextContent("从手机来")) }) {
		t.Fatal("not applied")
	}
	n.engine.CheckLocalClipboard()
	time.Sleep(300 * time.Millisecond)
	if len(p.received("clipboard_updated")) != 0 {
		t.Fatal("echoed back")
	}
	n.clipboard.userCopies(TextContent("from the pc"))
	n.engine.CheckLocalClipboard()
	if m := p.expect("clipboard_updated"); m.content() != "from the pc" {
		t.Fatal(m.content())
	}
	p.send(ClipboardGet())
	if m := p.expect("clipboard_content"); m.content() != "from the pc" {
		t.Fatal(m.content())
	}
}

func TestClipboardImagesBothWays(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	fromPhone := randomPNG(100_000)
	p.sendFile(fromPhone, "clipboard.png", "set")
	p.expect("file_status")
	if !eventually(5*time.Second, func() bool { return n.clipboard.holds(PNGContent(fromPhone)) }) {
		t.Fatal("image not applied")
	}
	fromPC := randomPNG(600_000)
	n.clipboard.userCopies(PNGContent(fromPC))
	p.send(ClipboardGet())
	if m := p.expect("file_start"); m.Clipboard != "content" {
		t.Fatal(m.Clipboard)
	}
	p.expect("file_end")
	var got []byte
	for _, c := range p.received("file_chunk") {
		b, _ := decodeB64(c.Data)
		got = append(got, b...)
	}
	if !bytes.Equal(got, fromPC) {
		t.Fatal("image differs")
	}
}

func TestFileGoesToDownloadsWithASafeName(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	body := pattern(200_000)
	p.sendFile(body, `..\..\evil\report.pdf`, "")
	if m := p.expect("file_status"); m.Status != "success" {
		t.Fatal(m.Status)
	}
	got, err := os.ReadFile(filepath.Join(n.downloads(), "report.pdf"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("file differs", err)
	}
}

func TestShortFileIsDiscarded(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	p.send(FileStart("x.bin", 10, "T1", ""))
	p.send(FileChunk("T1", 0, b64([]byte{1, 2, 3})))
	p.send(FileEnd("T1"))
	p.expect("error")
	if names := listDir(t, n.downloads()); len(names) != 0 {
		t.Fatal(names)
	}
}

func TestOpenURLOnlyForHTTP(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "smb://host/share", "https://"} {
		p.send(OpenURL(bad))
	}
	p.send(OpenURL("https://example.com/a?b=c"))
	eventually(5*time.Second, func() bool { n.delegate.mu.Lock(); defer n.delegate.mu.Unlock(); return len(n.delegate.urls) == 1 })
	time.Sleep(200 * time.Millisecond)
	n.delegate.mu.Lock()
	defer n.delegate.mu.Unlock()
	if len(n.delegate.urls) != 1 || n.delegate.urls[0] != "https://example.com/a?b=c" {
		t.Fatal(n.delegate.urls)
	}
}

func TestForgottenPhoneSayingHelloOpensAPrompt(t *testing.T) {
	n, p := phoneSetup(t)
	p.send(Hello("clipboard_image"))
	if m := p.expect("error"); m.Message != "unauthorized" {
		t.Fatal(m.Message)
	}
	if !eventually(5*time.Second, func() bool { return n.delegate.requestCount() == 1 }) {
		t.Fatal("no prompt")
	}
	n.engine.Approve(p.identity.Fingerprint)
	p.expect("pair_accepted")
}

func TestShareExtensionStyleConnection(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	p.disconnect()
	p.connect(n.port, true)
	p.send(ClipboardSet("shared text"))
	if !eventually(5*time.Second, func() bool { return n.clipboard.holds(TextContent("shared text")) }) {
		t.Fatal("not applied")
	}
}

// The share extension of the iPhone app (Copy to Clipboard): a second
// connection from the paired phone, while the app's own one is up. It sends
// an image only when our hello lists clipboard_image, and waits for
// file_status.
func TestShareExtensionCopiesAnImage(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	ext := &fakePhone{t: t, identity: p.identity, queue: NewQueue(nil)}
	t.Cleanup(func() { ext.disconnect(); ext.queue.Stop() })
	ext.connect(n.port, true)
	ext.send(Hello("clipboard_image"))
	if m := ext.expect("hello"); !slices.Contains(m.Features, "clipboard_image") {
		t.Fatal(m.Features)
	}
	png := randomPNG(200_000)
	ext.sendFile(png, "clipboard.png", "set")
	if m := ext.expect("file_status"); m.Status != "success" {
		t.Fatal(m.Status)
	}
	if !eventually(5*time.Second, func() bool { return n.clipboard.holds(PNGContent(png)) }) {
		t.Fatal("not on the clipboard")
	}
}

func TestStopClosesSessionsSoPeersSeeIt(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	n.engine.Stop()
	if !eventually(2*time.Second, func() bool { return !p.ready.Load() }) {
		t.Fatal("the peer did not see the close")
	}
}

func TestSendFileToPhone(t *testing.T) {
	n, p := phoneSetup(t)
	p.pair(n)
	data := pattern(700_000)
	path := filepath.Join(n.root, "photo.jpg")
	os.WriteFile(path, data, 0o644)
	n.engine.SendFiles([]string{path}, p.identity.Fingerprint)
	if m := p.expect("file_start"); m.Filename != "photo.jpg" || m.Clipboard != "" {
		t.Fatal(m)
	}
	p.expect("file_end")
	var got []byte
	for _, c := range p.received("file_chunk") {
		b, _ := decodeB64(c.Data)
		got = append(got, b...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("file differs")
	}
	// The phone sends no file_status; the send counts as done anyway.
	if !eventually(5*time.Second, func() bool { s := n.delegate.sentList(); return len(s) == 1 && s[0].ok }) {
		t.Fatal(n.delegate.sentList())
	}
}

// Several engines on one machine, standing in for a PC, a Mac and Linux.

func computers(t *testing.T, names ...string) []*testNode {
	var nodes []*testNode
	for _, name := range names {
		n := newNode(t, name)
		n.delegate.setAutoApprove(true)
		nodes = append(nodes, n)
	}
	return nodes
}

func pairComputers(t *testing.T, a, b *testNode) {
	t.Helper()
	a.engine.PairWithComputer("127.0.0.1", b.port)
	if !eventually(5*time.Second, func() bool { return a.delegate.has("accepted") }) {
		t.Fatal("pairing did not finish")
	}
	if !eventually(5*time.Second, func() bool { return a.connectedTo(b) && b.connectedTo(a) }) {
		t.Fatal("no session after pairing")
	}
}

func TestTwoComputersSyncBothWays(t *testing.T) {
	n := computers(t, "PC", "Mac")
	pairComputers(t, n[0], n[1])
	n[0].clipboard.userCopies(TextContent("copied on the pc"))
	n[0].engine.CheckLocalClipboard()
	if !eventually(5*time.Second, func() bool { return n[1].clipboard.holds(TextContent("copied on the pc")) }) {
		t.Fatal("pc to mac")
	}
	n[1].clipboard.userCopies(TextContent("copied on the mac"))
	n[1].engine.CheckLocalClipboard()
	if !eventually(5*time.Second, func() bool { return n[0].clipboard.holds(TextContent("copied on the mac")) }) {
		t.Fatal("mac to pc")
	}
	time.Sleep(500 * time.Millisecond)
	if n[0].clipboard.writeCount() != 1 || n[1].clipboard.writeCount() != 1 {
		t.Fatal("bounced")
	}
}

func TestImagesSyncBetweenComputers(t *testing.T) {
	n := computers(t, "PC", "Mac")
	pairComputers(t, n[0], n[1])
	png := randomPNG(700_000)
	n[0].clipboard.userCopies(PNGContent(png))
	n[0].engine.CheckLocalClipboard()
	if !eventually(10*time.Second, func() bool { return n[1].clipboard.holds(PNGContent(png)) }) {
		t.Fatal("image did not arrive")
	}
}

func TestTriangleDoesNotLoop(t *testing.T) {
	n := computers(t, "PC", "Mac", "Linux")
	pairComputers(t, n[0], n[1])
	pairComputers(t, n[1], n[2])
	pairComputers(t, n[2], n[0])
	text := TextContent("round and round")
	n[0].clipboard.userCopies(text)
	n[0].engine.CheckLocalClipboard()
	if !eventually(5*time.Second, func() bool { return n[1].clipboard.holds(text) && n[2].clipboard.holds(text) }) {
		t.Fatal("did not spread")
	}
	time.Sleep(time.Second)
	if n[0].clipboard.writeCount() != 0 || n[1].clipboard.writeCount() != 1 || n[2].clipboard.writeCount() != 1 {
		t.Fatal(n[0].clipboard.writeCount(), n[1].clipboard.writeCount(), n[2].clipboard.writeCount())
	}
}

// A certificate name that neither the iPhone app nor tetherd uses: the
// engine falls back to its guess, and corrects it when the peer sends
// clipboard_updated, as only computers do.
func TestComputerApprovedAsAPhoneIsCorrected(t *testing.T) {
	n := computers(t, "PC")
	n[0].delegate.setAutoApprove(false)
	linux := newPeer(t, "Another Tether")
	linux.connect(n[0].port, true)
	linux.send(PairRequest("Linux"))
	linux.expect("pair_pending")
	eventually(5*time.Second, func() bool { return n[0].delegate.requestCount() == 1 })
	n[0].engine.Approve(linux.identity.Fingerprint)
	linux.expect("pair_accepted")
	linux.send(TextMessage("clipboard_updated", "copied on linux"))
	if !eventually(5*time.Second, func() bool { return n[0].clipboard.holds(TextContent("copied on linux")) }) {
		t.Fatal("not applied")
	}
	linux.expect("hello")
	n[0].clipboard.userCopies(TextContent("copied on the pc"))
	n[0].engine.CheckLocalClipboard()
	if m := linux.expect("clipboard_set"); m.content() != "copied on the pc" {
		t.Fatal(m.content())
	}
}

func TestCertificateTellsAPhoneFromAComputer(t *testing.T) {
	for _, c := range []struct {
		certName string
		want     PeerKind
	}{{phoneCertName, Phone}, {computerCertName, Computer}} {
		n := newNode(t, "PC")
		p := newPeer(t, c.certName)
		p.connect(n.port, true)
		p.send(PairRequest("peer"))
		p.expect("pair_pending")
		if !eventually(5*time.Second, func() bool { return n.delegate.requestCount() == 1 }) {
			t.Fatal("no request")
		}
		if k := n.delegate.firstRequest().SuggestedKind; k != c.want {
			t.Fatalf("%s: %s, want %s", c.certName, k, c.want)
		}
	}
}

// A pairing from before the certificate rule, with the wrong kind, is
// corrected when the peer connects again.
func TestWrongKindIsCorrectedOnConnect(t *testing.T) {
	n, p := phoneSetup(t)
	p.send(PairRequest("Test iPhone"))
	p.expect("pair_pending")
	eventually(5*time.Second, func() bool { return n.delegate.requestCount() == 1 })
	n.engine.Approve(p.identity.Fingerprint)
	p.expect("pair_accepted")
	p.disconnect()
	fp := p.identity.Fingerprint
	n.engine.queue.Run(func() {
		known, _ := n.engine.knownHosts.Get(fp)
		known.Kind = Computer
		n.engine.knownHosts.Set(fp, known)
	}, time.Second)

	p.connect(n.port, true)
	if !eventually(5*time.Second, func() bool {
		for _, peer := range n.engine.SnapshotNow().Peers {
			if peer.Connected && peer.Kind == Phone {
				return true
			}
		}
		return false
	}) {
		t.Fatal("still a computer")
	}
	n.clipboard.userCopies(TextContent("to the phone"))
	n.engine.CheckLocalClipboard()
	if m := p.expect("clipboard_updated"); m.content() != "to the phone" {
		t.Fatal(m.content())
	}
}

func TestNetworkChangeReconnectsAtOnce(t *testing.T) {
	n := computers(t, "PC", "Mac")
	pairComputers(t, n[0], n[1])
	n[0].engine.NetworkDidChange()
	if !eventually(3*time.Second, func() bool { return n[0].connectedTo(n[1]) && n[1].connectedTo(n[0]) }) {
		t.Fatal("no session after the change")
	}
	n[0].clipboard.userCopies(TextContent("after the change"))
	n[0].engine.CheckLocalClipboard()
	if !eventually(5*time.Second, func() bool { return n[1].clipboard.holds(TextContent("after the change")) }) {
		t.Fatal("no sync after the change")
	}
}

// movePeer records a new port for a peer in n's known hosts, on n's queue.
func movePeer(n, peer *testNode, port int) {
	fp := peer.engine.Identity.Fingerprint
	n.engine.queue.Run(func() {
		known, _ := n.engine.knownHosts.Get(fp)
		known.Port = port
		n.engine.knownHosts.Set(fp, known)
	}, time.Second)
}

// A dual-boot laptop: Windows and Linux answer at the same address. When the
// Mac dials Windows and Linux answers, it must keep redialling Windows.
func TestWrongPeerAtTheAddressKeepsTheRedial(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	mac := newNode(t, "Mac", func(c *Config) {
		c.Log = func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }
	})
	mac.delegate.setAutoApprove(true)
	n := computers(t, "Windows", "Linux")
	win, linux := n[0], n[1]
	pairComputers(t, mac, win)
	pairComputers(t, mac, linux)

	// Windows goes away. The redial that this schedules finds Linux at
	// the same address. The Mac has forgotten Linux, so it does not know
	// that Linux holds the address (see the next test).
	mac.engine.Forget(linux.engine.Identity.Fingerprint)
	movePeer(mac, win, linux.port)
	win.engine.Stop()
	mismatch := func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			if strings.HasSuffix(l, "fingerprint mismatch") {
				return true
			}
		}
		return false
	}
	if !eventually(5*time.Second, mismatch) {
		t.Fatal("linux did not answer for windows")
	}

	// The laptop restarts into Windows. Windows has no address for the
	// Mac, so only a redial of Windows can bring the session back.
	linux.engine.Stop()
	win2 := newNode(t, "Windows", func(c *Config) {
		c.SupportDir = filepath.Join(win.root, "support")
		c.Port = linux.port
	})
	if !eventually(10*time.Second, func() bool { return mac.connectedTo(win2) }) {
		t.Fatal("the mac stopped redialling windows")
	}
}

// The dual-boot laptop again, now booted into Windows, with the Mac
// connected to it. The Mac's redial of Linux must not dial the address of
// Windows: Windows would take that session for a duplicate and drop the
// good one.
func TestRedialSkipsTheAddressOfAConnectedComputer(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	mac := newNode(t, "Mac", func(c *Config) {
		c.Log = func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }
	})
	mac.delegate.setAutoApprove(true)
	n := computers(t, "Windows", "Linux")
	win, linux := n[0], n[1]
	pairComputers(t, mac, win)
	pairComputers(t, mac, linux)

	movePeer(mac, linux, win.port)
	linux.engine.Stop()
	mu.Lock()
	lines = nil
	mu.Unlock()

	// The redials of Linux come after 2 and 4 seconds.
	time.Sleep(7 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "fingerprint mismatch") || strings.Contains(l, win.engine.Identity.Fingerprint[:12]) {
			t.Fatalf("the session with Windows was disturbed: %q", l)
		}
	}
	if !mac.connectedTo(win) {
		t.Fatal("not connected to Windows")
	}
}

// A computer on Ethernet and Wi-Fi has 2 addresses, and only 1 may answer
// from here. The address that worked last is tried first; mDNS addresses
// come after it.
func TestDialTriesEveryAddress(t *testing.T) {
	for _, c := range []struct {
		name            string
		known, mdnsHost string
		mdnsPort        func(win *testNode) int
	}{
		// mDNS gives an address that does not answer (TEST-NET-1).
		{"last address first", "", "192.0.2.1", func(win *testNode) int { return win.port }},
		// The last address no longer answers; mDNS gives the right one.
		{"falls back to mDNS", "127.0.0.1:1", "127.0.0.1", func(win *testNode) int { return win.port }},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := computers(t, "Mac", "Windows")
			mac, win := n[0], n[1]
			pairComputers(t, mac, win)
			fp := win.engine.Identity.Fingerprint
			mac.engine.queue.Run(func() {
				if c.known != "" {
					known, _ := mac.engine.knownHosts.Get(fp)
					host, port, _ := net.SplitHostPort(c.known)
					known.Host, known.Port = host, mustAtoi(port)
					mac.engine.knownHosts.Set(fp, known)
				}
				mac.engine.discovered[fp] = Discovered{Name: "Windows", Fingerprint: fp, Hosts: []string{c.mdnsHost},
					Port: c.mdnsPort(win), IsComputer: true}
			}, time.Second)
			mac.engine.NetworkDidChange()
			time.Sleep(200 * time.Millisecond)
			if !eventually(6*time.Second, func() bool { return mac.connectedTo(win) && win.connectedTo(mac) }) {
				t.Fatal("no session")
			}
		})
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// A Tether Desktop older than MinPeerVersion pairs, but every session with
// it closes at its hello, and the app is told.
func TestOutdatedPeerIsClosed(t *testing.T) {
	before := MinPeerVersion
	MinPeerVersion = "99.0.0" // higher than this build's Version
	t.Cleanup(func() { MinPeerVersion = before })
	n := computers(t, "Mac", "Windows")
	n[0].engine.PairWithComputer("127.0.0.1", n[1].port)
	if !eventually(5*time.Second, func() bool {
		n[0].delegate.mu.Lock()
		defer n[0].delegate.mu.Unlock()
		return len(n[0].delegate.outdated) > 0
	}) {
		t.Fatal("not told")
	}
	time.Sleep(300 * time.Millisecond)
	if n[0].connectedTo(n[1]) || n[1].connectedTo(n[0]) {
		t.Fatal("still connected")
	}
}

func TestUnansweredPairingTimesOut(t *testing.T) {
	mac := newNode(t, "Mac", func(c *Config) { c.PairingTimeout = time.Second })
	other := newNode(t, "Linux")
	mac.engine.PairWithComputer("127.0.0.1", other.port)
	if !eventually(5*time.Second, func() bool { return mac.delegate.has("failed") }) {
		t.Fatal("did not time out")
	}
	if len(mac.engine.SnapshotNow().Outgoing) != 0 {
		t.Fatal("still waiting")
	}
}

func TestSendFilesToAComputerInOrder(t *testing.T) {
	n := computers(t, "PC", "Mac")
	pairComputers(t, n[0], n[1])
	var paths []string
	for _, f := range []struct {
		name string
		size int
	}{{"a.bin", 1_000_000}, {"b.txt", 10}, {"c.pdf", 300_000}, {"empty.txt", 0}} {
		path := filepath.Join(n[0].root, f.name)
		os.WriteFile(path, pattern(f.size), 0o644)
		paths = append(paths, path)
	}
	paths = append(paths, n[0].root) // a folder fails, the rest goes on
	n[0].engine.SendFiles(paths, n[1].engine.Identity.Fingerprint)
	if !eventually(10*time.Second, func() bool { return len(n[0].delegate.sentList()) == 5 }) {
		t.Fatal(n[0].delegate.sentList())
	}
	got := n[0].delegate.sentList()
	for i, want := range []string{"a.bin", "b.txt", "c.pdf", "empty.txt"} {
		if got[i].name != want || !got[i].ok {
			t.Fatalf("%d: %+v", i, got[i])
		}
	}
	if got[4].ok {
		t.Fatal("a folder was sent")
	}
	if info, err := os.Stat(filepath.Join(n[1].downloads(), "c.pdf")); err != nil || info.Size() != 300_000 {
		t.Fatal("c.pdf", err)
	}
	if info, err := os.Stat(filepath.Join(n[1].downloads(), "empty.txt")); err != nil || info.Size() != 0 {
		t.Fatal("empty.txt", err)
	}
}
