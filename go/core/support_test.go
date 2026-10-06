package core

import (
	"crypto/rand"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClipboard struct {
	mu      sync.Mutex
	count   uint64
	content Content
	has     bool
	writes  int
}

func (c *fakeClipboard) ChangeCount() uint64 { c.mu.Lock(); defer c.mu.Unlock(); return c.count }

func (c *fakeClipboard) Read() (Content, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.content, c.has
}

func (c *fakeClipboard) Write(v Content) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content, c.has = v, true
	c.writes++
	c.count++
	return c.count
}

// userCopies is a copy made by the user on this machine.
func (c *fakeClipboard) userCopies(v Content) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.content, c.has = v, true
	c.count++
}

func (c *fakeClipboard) writeCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.writes }

func (c *fakeClipboard) holds(v Content) bool {
	got, ok := c.Read()
	return ok && got.Equal(v)
}

type sent struct {
	name string
	ok   bool
}

type recordingDelegate struct {
	mu          sync.Mutex
	requests    []PairingRequest
	urls        []string
	files       []string
	outbound    []OutboundResult
	sent        []sent
	autoApprove bool
	outdated    []string
	engine      *Engine
}

func (d *recordingDelegate) PairingRequested(r PairingRequest) {
	d.mu.Lock()
	d.requests = append(d.requests, r)
	approve, e := d.autoApprove, d.engine
	d.mu.Unlock()
	if approve {
		e.Approve(r.Fingerprint)
	}
}
func (d *recordingDelegate) PairingResolved(string) {}
func (d *recordingDelegate) OutboundPairing(r OutboundResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.outbound = append(d.outbound, r)
}
func (d *recordingDelegate) OpenURL(u string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.urls = append(d.urls, u)
}
func (d *recordingDelegate) FileReceived(p, _ string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files = append(d.files, p)
}
func (d *recordingDelegate) FileSent(name, _ string, ok bool, _ string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, sent{name, ok})
}
func (d *recordingDelegate) Failed(m string) { panic("engine failed: " + m) }
func (d *recordingDelegate) StateChanged()   {}
func (d *recordingDelegate) PeerOutdated(name, version string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.outdated = append(d.outdated, name+" "+version)
}

func (d *recordingDelegate) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}
func (d *recordingDelegate) firstRequest() PairingRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requests[0]
}
func (d *recordingDelegate) sentList() []sent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sent(nil), d.sent...)
}
func (d *recordingDelegate) has(kind string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.outbound {
		if r.Kind == kind {
			return true
		}
	}
	return false
}
func (d *recordingDelegate) setAutoApprove(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.autoApprove = on
}

var nextPort atomic.Int64

func init() {
	n, _ := rand.Int(rand.Reader, big.NewInt(10000))
	nextPort.Store(30000 + n.Int64())
}

// testNode is a temporary home for one engine: identity, known hosts, downloads.
type testNode struct {
	root      string
	clipboard *fakeClipboard
	delegate  *recordingDelegate
	engine    *Engine
	port      int
}

func newNode(t *testing.T, name string, configure ...func(*Config)) *testNode {
	t.Helper()
	root := t.TempDir()
	port := int(nextPort.Add(1))
	cfg := Config{SupportDir: filepath.Join(root, "support"), DownloadsDir: filepath.Join(root, "downloads"), DeviceName: name, Port: port}
	for _, f := range configure {
		f(&cfg)
	}
	n := &testNode{root: root, clipboard: &fakeClipboard{}, delegate: &recordingDelegate{}, port: port}
	e, err := NewEngine(cfg, n.clipboard, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Delegate = n.delegate
	n.delegate.engine = e
	n.engine = e
	e.Start()
	time.Sleep(150 * time.Millisecond)
	t.Cleanup(e.Stop)
	return n
}

func (n *testNode) downloads() string { return filepath.Join(n.root, "downloads") }

func (n *testNode) connectedTo(o *testNode) bool {
	for _, p := range n.engine.SnapshotNow().Peers {
		if p.Fingerprint == o.engine.Identity.Fingerprint && p.Connected {
			return true
		}
	}
	return false
}

// fakePhone behaves like the iPhone app: dials, pairs, sends commands.
type fakePhone struct {
	t        *testing.T
	identity *Identity
	queue    *Queue
	conn     *Conn
	mu       sync.Mutex
	inbox    []Message
	ready    atomic.Bool
}

func newPhone(t *testing.T) *fakePhone { return newPeer(t, phoneCertName) }

// newPeer is a fake phone with another name in its certificate.
func newPeer(t *testing.T, certName string) *fakePhone {
	t.Helper()
	dir := t.TempDir()
	pfx, err := createIdentity(certName, "")
	if err == nil {
		err = WritePrivate(filepath.Join(dir, identityFile), pfx)
	}
	if err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateIdentity(dir, FilePermissionsOnly{})
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePhone{t: t, identity: id, queue: NewQueue(nil)}
	t.Cleanup(func() { p.disconnect(); p.queue.Stop() })
	return p
}

func (p *fakePhone) connect(port int, expectReady bool) {
	c := Dial("127.0.0.1", port, p.identity, p.queue)
	c.OnReady = func(*Conn) { p.ready.Store(true) }
	c.OnMessage = func(_ *Conn, m Message) { p.mu.Lock(); p.inbox = append(p.inbox, m); p.mu.Unlock() }
	c.OnClose = func(*Conn, string) { p.ready.Store(false) }
	p.queue.Run(func() { p.conn = c; c.Start() }, time.Second)
	if expectReady && !eventually(5*time.Second, p.ready.Load) {
		p.t.Fatal("phone could not connect")
	}
}

func (p *fakePhone) disconnect() {
	p.queue.Run(func() {
		if p.conn != nil {
			p.conn.Close("bye")
		}
	}, 2*time.Second)
}

func (p *fakePhone) send(m Message) {
	p.queue.Post(func() {
		if p.conn != nil {
			p.conn.Send(m, nil)
		}
	})
}

func (p *fakePhone) serverFingerprint() string {
	var fp string
	p.queue.Run(func() { fp = p.conn.Fingerprint }, time.Second)
	return fp
}

// expect waits for the next message with this command and removes it.
func (p *fakePhone) expect(command string) Message {
	p.t.Helper()
	var found *Message
	eventually(5*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		for i, m := range p.inbox {
			if m.Command == command {
				found = &m
				p.inbox = append(p.inbox[:i], p.inbox[i+1:]...)
				return true
			}
		}
		return false
	})
	if found == nil {
		p.t.Fatalf("no %s arrived", command)
	}
	return *found
}

func (p *fakePhone) received(command string) []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	var list []Message
	for _, m := range p.inbox {
		if m.Command == command {
			list = append(list, m)
		}
	}
	return list
}

// pair goes through the engine's prompt, which the test approves.
func (p *fakePhone) pair(n *testNode) {
	p.t.Helper()
	p.send(PairRequest("Test iPhone"))
	p.expect("pair_pending")
	if !eventually(5*time.Second, func() bool { return n.delegate.requestCount() > 0 }) {
		p.t.Fatal("no prompt")
	}
	n.engine.Approve(p.identity.Fingerprint)
	p.expect("pair_accepted")
	p.send(Hello("clipboard_image"))
	p.expect("hello")
}

func (p *fakePhone) sendFile(data []byte, name, clipboard string) {
	id := randomHex(8)
	p.send(FileStart(name, int64(len(data)), id, clipboard))
	index := 0
	for off := 0; off < len(data); off += 48 * 1024 {
		end := min(off+48*1024, len(data))
		p.send(FileChunk(id, index, b64(data[off:end])))
		index++
	}
	p.send(FileEnd(id))
}

func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func randomPNG(n int) []byte {
	data := make([]byte, 8+n)
	copy(data, pngSignature)
	_, _ = rand.Read(data[8:])
	return data
}

func pattern(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i * 31)
	}
	return data
}

func listDir(t *testing.T, dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Messages only the fake iPhone sends.
func ClipboardSet(text string) Message { return Message{Command: "clipboard_set", Content: &text} }
func ClipboardGet() Message            { return Message{Command: "clipboard_get"} }
func OpenURL(url string) Message       { return Message{Command: "open_url", Content: &url} }
