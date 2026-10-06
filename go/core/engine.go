package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Config is how the app sets up the engine.
type Config struct {
	SupportDir   string
	DownloadsDir string
	// The identity file and its password; identity.pfx in SupportDir, with
	// no password, when empty.
	IdentityFile     string
	IdentityPassword string
	DeviceName       string
	Port             int
	// How long a pairing request to a computer waits for its user.
	PairingTimeout time.Duration
	Protector      SecretProtector
	MarkDownloaded func(path string)
	// Never given clipboard or file content.
	Log func(string)
}

var features = []string{"clipboard_image"}

const (
	imageChunkBytes    = 256 * 1024
	fileChunkBytes     = 256 * 1024
	maxPendingRequests = 3
	fileStatusTimeout  = 30 * time.Second
	// MaxPhoneFileBytes: the iPhone app keeps a file it receives in memory
	// until the end.
	MaxPhoneFileBytes = 512 << 20
)

type state int

const (
	dialing state = iota
	unpaired
	pairingOut
	paired
)

type session struct {
	conn         *Conn
	state        state
	kind         PeerKind
	name         string
	helloSent    bool
	peerFeatures map[string]bool
	expectedFP   string
	// Files go out one at a time; the rest wait here.
	sendQueue []string
	sending   *outgoingFile
}

func (s *session) fp() string { return s.conn.Fingerprint }

type outgoingFile struct {
	id              string
	path            string
	file            *os.File
	size, sent      int64
	chunks          int
	reportedPercent int
}

func (f *outgoingFile) name() string { return filepath.Base(f.path) }

type pending struct {
	name    string
	session string
	created time.Time
}

// Engine is the desktop side of the Tether protocol: it listens and
// advertises like tetherd, answers phones, and keeps sessions with paired
// computers to sync the clipboard.
//
// All state lives on its queue. Public methods may be called from any goroutine.
type Engine struct {
	Identity *Identity
	Config   Config
	Delegate Delegate

	queue      *Queue
	stopped    atomic.Bool
	clipboard  Clipboard
	discovery  Discovery
	knownHosts *KnownHosts
	files      *FileReceiver
	log        func(string)

	listener      net.Listener
	listening     bool
	sessions      map[string]*session
	pending       map[string]pending
	rejected      map[string]bool // dropped at once for the rest of the run
	discovered    map[string]Discovered
	redialDelay   map[string]time.Duration
	redialPending map[string]bool

	// Loop prevention: the content last applied or sent, and the clipboard
	// change count our own last write produced.
	lastSyncedDigest string
	ownChangeCount   uint64
	lastSeenCount    uint64
	haveOwnCount     bool
}

func NewEngine(cfg Config, clipboard Clipboard, discovery Discovery) (*Engine, error) {
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.PairingTimeout == 0 {
		cfg.PairingTimeout = 3 * time.Minute
	}
	if cfg.Protector == nil {
		cfg.Protector = FilePermissionsOnly{}
	}
	if cfg.Log == nil {
		cfg.Log = func(string) {}
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName, _ = os.Hostname()
	}
	if cfg.IdentityFile == "" {
		cfg.IdentityFile = filepath.Join(cfg.SupportDir, identityFile)
	}
	identity, err := LoadOrCreateIdentityFile(cfg.IdentityFile, cfg.IdentityPassword, cfg.Protector)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		Identity: identity, Config: cfg, clipboard: clipboard, discovery: discovery, log: cfg.Log,
		knownHosts:    LoadKnownHosts(filepath.Join(cfg.SupportDir, "known_hosts.json"), cfg.Log),
		files:         NewFileReceiver(filepath.Join(cfg.SupportDir, "incoming"), cfg.DownloadsDir, DefaultFileLimits, cfg.MarkDownloaded),
		sessions:      map[string]*session{},
		pending:       map[string]pending{},
		rejected:      map[string]bool{},
		discovered:    map[string]Discovered{},
		redialDelay:   map[string]time.Duration{},
		redialPending: map[string]bool{},
	}
	e.queue = NewQueue(func(r any) { e.log(fmt.Sprintf("engine error: %v", r)) })
	return e, nil
}

// MARK: - Public API

func (e *Engine) Start() {
	e.queue.Post(func() {
		e.lastSeenCount = e.clipboard.ChangeCount()
		e.startListener()
		if e.discovery != nil {
			e.discovery.Start(e.Config.DeviceName, e.Config.Port, e.Identity.Fingerprint, func(results []Discovered) {
				e.queue.Post(func() { e.updateDiscovered(results) })
			})
		}
		for fp, host := range e.knownHosts.All() {
			if host.Kind == Computer {
				e.dialComputer(fp)
			}
		}
	})
}

// Stop closes every session with a FIN, so peers drop them at once. tetherd
// before 0.2.36 has no keepalive: a session left open there hangs it at its
// next large write. It waits at most 1 second.
func (e *Engine) Stop() {
	if !e.stopped.CompareAndSwap(false, true) {
		return
	}
	e.queue.Run(func() {
		if e.listener != nil {
			e.listener.Close()
			e.listener = nil
		}
		e.listening = false
		if e.discovery != nil {
			e.discovery.Stop()
		}
		for _, s := range e.sessionList() {
			s.conn.Close("stopping")
		}
	}, time.Second)
	// Let the FINs leave.
	time.Sleep(100 * time.Millisecond)
	e.queue.Stop()
}

// Snapshot calls completion on the engine's queue with the current state.
func (e *Engine) Snapshot(completion func(Snapshot)) {
	e.queue.Post(func() { completion(e.makeSnapshot()) })
}

// SnapshotNow waits for the state. Never call it from a thread the
// clipboard needs: the engine may be waiting for that thread.
func (e *Engine) SnapshotNow() Snapshot {
	var snap Snapshot
	e.queue.Run(func() { snap = e.makeSnapshot() }, 5*time.Second)
	return snap
}

// Approve pins a peer that asked to pair, and tells it so if it is still connected.
func (e *Engine) Approve(fp string) {
	e.queue.Post(func() {
		request, ok := e.pending[fp]
		if !ok {
			return
		}
		kind := e.kindOf(fp)
		delete(e.pending, fp)
		e.knownHosts.Set(fp, KnownHost{Name: request.name, Kind: kind})
		e.log(fmt.Sprintf("paired with %s as %s", request.name, kind))
		if s, ok := e.sessions[request.session]; ok && !s.conn.Closed {
			s.conn.Send(PairAccepted(), nil)
			e.promote(s, kind, request.name)
		}
		// Our own request to the same computer is settled now; the peer keeps
		// that session unpromoted, so drop it.
		for _, other := range e.sessionList() {
			if other.state == pairingOut && other.fp() == fp {
				other.state = unpaired
				other.conn.Close("paired from the other side")
				e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "accepted", Peer: request.name}) })
			}
		}
		e.notify(func(d Delegate) { d.PairingResolved(fp) })
		e.notifyState()
	})
}

func (e *Engine) Reject(fp string) {
	e.queue.Post(func() {
		request, ok := e.pending[fp]
		if !ok {
			return
		}
		delete(e.pending, fp)
		e.rejected[fp] = true
		if s, ok := e.sessions[request.session]; ok {
			conn := s.conn
			conn.Send(Message{Command: "pair_rejected"}, func(bool) { conn.Close("pairing rejected") })
		}
		e.notify(func(d Delegate) { d.PairingResolved(fp) })
		e.notifyState()
	})
}

func (e *Engine) Forget(fp string) {
	e.queue.Post(func() {
		if !e.knownHosts.Remove(fp) {
			return
		}
		for _, s := range e.sessionList() {
			if s.fp() == fp {
				s.conn.Close("forgotten")
			}
		}
		e.notifyState()
	})
}

// PairWithComputer asks a computer to pair. The other side approves it with
// `tether accept <our fingerprint>` (tetherd) or its own prompt.
func (e *Engine) PairWithComputer(host string, port int) {
	e.queue.Post(func() {
		s := e.adopt(Dial(host, port, e.Identity, e.queue), pairingOut, host)
		e.notifyState()
		e.queue.PostAfter(e.Config.PairingTimeout, func() {
			if s.state == pairingOut {
				s.conn.Close("nobody approved the request in time")
			}
		})
	})
}

func (e *Engine) PairWithDiscovered(fp string) {
	e.queue.Post(func() {
		if d, ok := e.discovered[fp]; ok && len(d.Hosts) > 0 {
			s := e.adopt(DialAny(addresses(d.Hosts, d.Port), e.Identity, e.queue), pairingOut, d.Name)
			s.kind = Computer
			e.notifyState()
		}
	})
}

// CancelOutboundPairing gives up every pairing request this desktop is waiting on.
func (e *Engine) CancelOutboundPairing() {
	e.queue.Post(func() {
		for _, s := range e.sessionList() {
			if s.state == pairingOut {
				// Not pairingOut any more, so closing reports no failure.
				s.state = unpaired
				s.conn.Close("pairing cancelled")
			}
		}
		e.notifyState()
	})
}

// NetworkDidChange: after a resume or a network change, sessions with
// computers may be dead without either side knowing. Drop them and dial again
// at once. Phones dial us themselves.
func (e *Engine) NetworkDidChange() {
	e.queue.Post(func() {
		e.log("network changed: reconnecting to computers")
		e.redialDelay = map[string]time.Duration{}
		for _, s := range e.sessionList() {
			if s.kind == Computer && s.state != pairingOut {
				s.conn.Close("network changed")
			}
		}
		for fp, host := range e.knownHosts.All() {
			if host.Kind == Computer {
				e.dialComputer(fp)
			}
		}
		e.notifyState()
	})
}

// SendFiles sends files to a connected peer, one after the other.
func (e *Engine) SendFiles(paths []string, fp string) {
	e.queue.Post(func() {
		var target *session
		for _, s := range e.sessionList() {
			if s.fp() == fp && s.state == paired && !s.conn.Closed {
				target = s
				break
			}
		}
		if target == nil {
			peer := "the device"
			if host, ok := e.knownHosts.Get(fp); ok {
				peer = host.Name
			}
			for _, path := range paths {
				e.reportSend(filepath.Base(path), peer, false, "it is not connected")
			}
			return
		}
		target.sendQueue = append(target.sendQueue, paths...)
		e.sendNextFile(target)
	})
}

// CheckLocalClipboard: the app calls this when the clipboard changed; it is
// cheap when nothing did.
func (e *Engine) CheckLocalClipboard() {
	e.queue.Post(func() {
		count := e.clipboard.ChangeCount()
		if count == e.lastSeenCount {
			return
		}
		e.lastSeenCount = count
		if e.haveOwnCount && count == e.ownChangeCount {
			return
		}
		content, ok := e.clipboard.Read()
		if !ok {
			return
		}
		if !content.Fits() {
			e.log("clipboard content is over the size limit; not sent")
			return
		}
		digest := content.Digest()
		if digest == e.lastSyncedDigest {
			return
		}
		e.lastSyncedDigest = digest
		e.broadcast(content, nil)
	})
}

// MARK: - Listening and discovery

func (e *Engine) startListener() {
	lc := net.ListenConfig{KeepAliveConfig: keepAlive}
	l, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf(":%d", e.Config.Port))
	if err != nil {
		e.fail(fmt.Sprintf("cannot listen on port %d: %v", e.Config.Port, err))
		e.notifyState()
		return
	}
	e.listener = l
	e.listening = true
	e.log(fmt.Sprintf("listening on port %d", e.Config.Port))
	go func() {
		for {
			raw, err := l.Accept()
			if err != nil {
				return
			}
			e.queue.Post(func() { e.adopt(Accept(raw, e.Identity, e.queue), unpaired, "") })
		}
	}()
	e.notifyState()
}

func (e *Engine) updateDiscovered(results []Discovered) {
	found := map[string]Discovered{}
	for _, r := range results {
		if r.Fingerprint != "" && r.Fingerprint != e.Identity.Fingerprint {
			if _, seen := found[r.Fingerprint]; !seen {
				found[r.Fingerprint] = r
			}
		}
	}
	e.discovered = found
	for fp := range found {
		if host, ok := e.knownHosts.Get(fp); ok && host.Kind == Computer {
			e.dialComputer(fp)
		}
	}
	e.notifyState()
}

// dialComputer keeps a session with every paired computer. tetherd never
// redials after a drop unless mDNS changes, so we always dial, whichever
// fingerprint is lower.
func (e *Engine) dialComputer(fp string) {
	known, ok := e.knownHosts.Get(fp)
	if !ok || known.Kind != Computer {
		return
	}
	for _, s := range e.sessions {
		if s.fp() == fp || s.expectedFP == fp {
			return
		}
	}
	// The address that worked last comes first, then the ones mDNS gives:
	// not every address of a computer answers from here.
	var candidates []string
	if known.Host != "" {
		candidates = addresses([]string{known.Host}, known.Port)
	}
	if d, ok := e.discovered[fp]; ok {
		candidates = append(candidates, addresses(d.Hosts, d.Port)...)
	}
	// On a dual-boot laptop, the other system may hold an address now.
	// Dialling it would reach that computer, which would take the session
	// for a duplicate and drop the one it has.
	var addrs []string
	inUse := e.addressesInUse(fp)
	for _, a := range candidates {
		if !inUse[a] && !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		if len(candidates) > 0 {
			e.scheduleRedial(fp)
		}
		return
	}
	s := e.adopt(DialAny(addrs, e.Identity, e.queue), dialing, known.Name)
	s.expectedFP = fp
	s.kind = Computer
}

// addresses makes host:port addresses, with the default port for 0.
func addresses(hosts []string, port int) []string {
	if port == 0 {
		port = DefaultPort
	}
	var out []string
	for _, h := range hosts {
		out = append(out, net.JoinHostPort(h, strconv.Itoa(port)))
	}
	return out
}

// addressesInUse returns the addresses where other connected computers
// listen.
func (e *Engine) addressesInUse(fp string) map[string]bool {
	inUse := map[string]bool{}
	for _, s := range e.sessions {
		other := s.fp()
		if s.state != paired || other == fp {
			continue
		}
		if k, ok := e.knownHosts.Get(other); ok && k.Host != "" {
			inUse[addresses([]string{k.Host}, k.Port)[0]] = true
		}
		if d, ok := e.discovered[other]; ok {
			for _, a := range addresses(d.Hosts, d.Port) {
				inUse[a] = true
			}
		}
	}
	return inUse
}

func (e *Engine) scheduleRedial(fp string) {
	if host, ok := e.knownHosts.Get(fp); !ok || host.Kind != Computer || e.redialPending[fp] {
		return
	}
	delay := e.redialDelay[fp]
	if delay == 0 {
		delay = 2 * time.Second
	}
	e.redialDelay[fp] = min(delay*2, time.Minute)
	e.redialPending[fp] = true
	e.queue.PostAfter(delay, func() {
		delete(e.redialPending, fp)
		e.dialComputer(fp)
	})
}

// MARK: - Sessions

func (e *Engine) sessionList() []*session {
	list := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		list = append(list, s)
	}
	return list
}

func (e *Engine) adopt(c *Conn, st state, name string) *session {
	s := &session{conn: c, state: st, name: name, peerFeatures: map[string]bool{}}
	e.sessions[c.ID] = s
	c.OnReady = e.ready
	c.OnMessage = e.received
	c.OnClose = e.closed
	c.Start()
	return s
}

func (e *Engine) ready(c *Conn) {
	s, ok := e.sessions[c.ID]
	if !ok {
		return
	}
	fp := c.Fingerprint
	if fp == e.Identity.Fingerprint || e.rejected[fp] {
		c.Close("refused")
		return
	}
	if s.expectedFP != "" && s.expectedFP != fp {
		c.Close("fingerprint mismatch")
		return
	}
	if known, ok := e.knownHosts.Get(fp); ok {
		updated := known
		if c.Direction == Outbound && c.RemoteHost != "" {
			updated.Host, updated.Port = c.RemoteHost, c.RemotePort
		}
		// Pairings from before the certificate rule may have the wrong kind.
		if kind, ok := KindFromCertName(c.CertName); ok && kind != known.Kind {
			e.log(fmt.Sprintf("%s has the certificate of a %s: changing its kind", known.Name, kind))
			updated.Kind = kind
		}
		if updated != known {
			e.knownHosts.Set(fp, updated)
		}
		e.promote(s, updated.Kind, updated.Name)
		return
	}
	if s.state == pairingOut {
		c.Send(PairRequest(e.Config.DeviceName), nil)
		e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "waiting"}) })
		return
	}
	// An unknown peer dialled us: wait for its pair_request, but not forever
	// unless it opened a prompt.
	s.state = unpaired
	e.queue.PostAfter(30*time.Second, func() {
		if s.state == unpaired && e.pending[fp].session != c.ID {
			c.Close("no pairing request")
		}
	})
}

// promote marks a session as trusted, closes a duplicate, and greets computers.
func (e *Engine) promote(s *session, kind PeerKind, name string) {
	s.state, s.kind, s.name = paired, kind, name
	fp := s.fp()
	delete(e.redialDelay, fp)
	for _, other := range e.sessionList() {
		if other != s && other.fp() == fp && other.state == paired {
			e.loser(s, other, kind).conn.Close("duplicate session")
		}
	}
	if s.conn.Closed {
		return
	}
	if kind == Computer && !s.helloSent {
		s.helloSent = true
		s.conn.Send(Hello(helloFeatures()...), nil)
	}
	e.log("session up: " + s.conn.String())
	e.notifyState()
}

// loser picks which of 2 sessions with one peer goes. Between computers, both
// sides keep the session that the lower fingerprint dialled, so they never
// both close one each. Otherwise the newer session wins: the older one is
// often a dead socket from a phone that left the network.
func (e *Engine) loser(newer, older *session, kind PeerKind) *session {
	if kind == Computer && newer.conn.Direction != older.conn.Direction {
		keepOutbound := e.Identity.Fingerprint < newer.fp()
		if (newer.conn.Direction == Outbound) == keepOutbound {
			return older
		}
		return newer
	}
	return older
}

func (e *Engine) closed(c *Conn, reason string) {
	s, ok := e.sessions[c.ID]
	if !ok {
		return
	}
	delete(e.sessions, c.ID)
	e.files.CloseSession(c.ID)
	if s.sending != nil {
		e.finishSend(s, s.sending, false, "the connection was lost")
	}
	for _, path := range s.sendQueue {
		e.reportSend(filepath.Base(path), s.name, false, "the connection was lost")
	}
	s.sendQueue = nil
	e.log("session closed: " + c.String() + " " + reason)
	if s.state == pairingOut {
		e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "failed", Reason: reason}) })
	}
	// Redial the computer we dialled, not the one that answered: on a
	// dual-boot laptop another peer can answer at the same address.
	fp := s.expectedFP
	if fp == "" {
		fp = s.fp()
	}
	if host, ok := e.knownHosts.Get(fp); ok && host.Kind == Computer {
		up := false
		for _, other := range e.sessions {
			if other.fp() == fp && other.state == paired {
				up = true
			}
		}
		if !up {
			e.scheduleRedial(fp)
		}
	}
	e.notifyState()
}

// MARK: - Messages

func (e *Engine) received(c *Conn, m Message) {
	s, ok := e.sessions[c.ID]
	if !ok {
		return
	}
	switch s.state {
	case unpaired:
		e.receivedUnpaired(m, s)
	case pairingOut:
		e.receivedWhilePairingOut(m, s)
	case paired:
		e.receivedPaired(m, s)
	}
}

func (e *Engine) receivedUnpaired(m Message, s *session) {
	fp := s.fp()
	if fp == "" {
		return
	}
	unauthorized := Message{Command: "error", Message: "unauthorized"}
	switch m.Command {
	case "pair_request":
		name := "Unknown device"
		if m.DeviceName != "" {
			name = truncate(m.DeviceName, 100)
		}
		e.askToPair(fp, name, s)
		s.conn.Send(Message{Command: "pair_pending", DeviceName: e.Config.DeviceName}, nil)
	case "hello":
		// A phone that still trusts us after we forgot it sends hello and
		// never pair_request again, so its fingerprint alone opens a prompt.
		s.conn.Send(unauthorized, nil)
		e.askToPair(fp, "Unknown device", s)
	default:
		s.conn.Send(unauthorized, nil)
	}
}

func (e *Engine) askToPair(fp, name string, s *session) {
	_, exists := e.pending[fp]
	// A new certificate is free to make, so cap the open prompts.
	if !exists && len(e.pending) >= maxPendingRequests {
		s.conn.Close("too many pairing requests")
		return
	}
	// Keep a real device name over the placeholder.
	kept := name
	if name == "Unknown device" && exists {
		kept = e.pending[fp].name
	}
	created := time.Now()
	e.pending[fp] = pending{name: kept, session: s.conn.ID, created: created}
	// A request nobody answers lapses after 10 minutes.
	e.queue.PostAfter(10*time.Minute, func() {
		if p, ok := e.pending[fp]; ok && p.created.Equal(created) {
			delete(e.pending, fp)
			e.notify(func(d Delegate) { d.PairingResolved(fp) })
			e.notifyState()
		}
	})
	if !exists {
		request := PairingRequest{Fingerprint: fp, Name: name, SuggestedKind: e.kindOf(fp)}
		e.notify(func(d Delegate) { d.PairingRequested(request) })
		e.notifyState()
	}
}

// kindOf tells a phone from a computer by its certificate. For a name that
// neither the iPhone app nor tetherd uses, it guesses: a peer that asks us
// to pair is a phone, unless we are asking it at the same time (two
// computers pairing from both ends) or it publishes v=1 over DNS-SD, as
// tetherd and Tether Desktop do and the phone does not.
func (e *Engine) kindOf(fp string) PeerKind {
	for _, s := range e.sessions {
		if s.fp() == fp {
			if kind, ok := KindFromCertName(s.conn.CertName); ok {
				return kind
			}
		}
	}
	for _, s := range e.sessions {
		if s.state == pairingOut && s.fp() == fp {
			return Computer
		}
	}
	if d, ok := e.discovered[fp]; ok && d.IsComputer {
		return Computer
	}
	return Phone
}

func (e *Engine) receivedWhilePairingOut(m Message, s *session) {
	switch m.Command {
	case "pair_pending":
		if m.DeviceName != "" {
			s.name = truncate(m.DeviceName, 100)
		}
		peer := s.name
		e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "waiting", Peer: peer}) })
		e.notifyState()
	case "pair_accepted":
		fp := s.fp()
		if fp == "" {
			return
		}
		// We asked it to pair, so it is a computer, unless its certificate
		// says otherwise.
		kind := Computer
		if k, ok := KindFromCertName(s.conn.CertName); ok {
			kind = k
		}
		e.knownHosts.Set(fp, KnownHost{Name: s.name, Kind: kind, Host: s.conn.RemoteHost, Port: s.conn.RemotePort})
		name := s.name
		e.log(fmt.Sprintf("paired with %s %s", kind, name))
		e.promote(s, kind, name)
		e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "accepted", Peer: name}) })
	case "pair_rejected":
		s.state = unpaired
		e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "rejected"}) })
		s.conn.Close("pairing rejected")
	case "error":
		s.state = unpaired
		detail := m.Message
		if detail == "" {
			detail = "error"
		}
		e.notify(func(d Delegate) { d.OutboundPairing(OutboundResult{Kind: "failed", Reason: detail}) })
		s.conn.Close("pairing error")
	}
}

func (e *Engine) receivedPaired(m Message, s *session) {
	c := s.conn
	switch m.Command {
	case "hello":
		s.peerFeatures = map[string]bool{}
		for _, f := range m.Features {
			s.peerFeatures[f] = true
		}
		// A Tether Desktop older than this release supports: it must be
		// updated before it syncs again.
		if v, ok := peerVersion(m.Features); ok && peerTooOld(v) {
			name := s.name
			e.log(fmt.Sprintf("%s runs version %s, older than %s: closing", name, v, MinPeerVersion))
			e.notify(func(d Delegate) { d.PeerOutdated(name, v) })
			c.Close("peer needs an update")
			return
		}
		// Answer a greeting once. A hello after ours is the answer to it.
		if !s.helloSent {
			s.helloSent = true
			c.Send(Hello(helloFeatures()...), nil)
		}
	case "clipboard_set", "clipboard_updated":
		// Phones never send clipboard_updated; tetherd does.
		if m.Command == "clipboard_updated" && s.kind == Phone {
			e.reclassifyAsComputer(s)
		}
		if text := m.content(); text != "" {
			e.apply(TextContent(text), s)
		}
	case "clipboard_get":
		e.answerClipboardGet(s)
	case "open_url":
		raw := m.content()
		if u, err := url.Parse(raw); err == nil && len(raw) <= 8192 &&
			(strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) && u.Host != "" {
			e.notify(func(d Delegate) { d.OpenURL(raw) })
		}
	case "new_otp":
		if m.OTP != "" && len(m.OTP) <= 64 {
			e.apply(TextContent(m.OTP), s)
		}
		c.Send(Message{Command: "status", Status: "ok"}, nil)
	case "file_start":
		if err := e.files.Start(c.ID, m.TransferID, m.Filename, m.Size, m.Clipboard); err != nil {
			e.transferFailed(s, m.TransferID, err)
		}
	case "file_chunk":
		if err := e.files.Chunk(c.ID, m.TransferID, m.Data); err != nil && !errors.Is(err, errUnknownTransfer) {
			e.transferFailed(s, m.TransferID, err)
		}
	case "file_end":
		e.finishTransfer(s, m.TransferID)
	case "file_status":
		if s.sending != nil && m.TransferID == s.sending.id {
			reason := m.Message
			if reason == "" {
				reason = m.Status
			}
			e.finishSend(s, s.sending, m.Status == "success", reason)
		}
	case "error":
		if s.sending != nil && m.TransferID == s.sending.id {
			reason := m.Message
			if reason == "" {
				reason = "the other side refused it"
			}
			e.finishSend(s, s.sending, false, reason)
		}
	case "pair_request":
		// Already pinned: the phone missed our pair_accepted.
		c.Send(PairAccepted(), nil)
		// pair_pending, pair_accepted, status and unknown commands need no
		// answer. Never answer with "OK".
	}
}

// reclassifyAsComputer: a computer that was approved as a phone gets the
// right kind here, and from then on gets clipboard_set, the only command
// tetherd applies. Only a certificate with an unknown name gets that far.
func (e *Engine) reclassifyAsComputer(s *session) {
	host, ok := e.knownHosts.Get(s.fp())
	if !ok {
		return
	}
	e.log(s.name + " sends clipboard_updated: treating it as a computer")
	host.Kind = Computer
	e.knownHosts.Set(s.fp(), host)
	s.kind = Computer
	if !s.helloSent {
		s.helloSent = true
		s.conn.Send(Hello(helloFeatures()...), nil)
	}
	e.notifyState()
}

// MARK: - Files in

func (e *Engine) finishTransfer(s *session, id string) {
	outcome, err := e.files.End(s.conn.ID, id)
	if errors.Is(err, errUnknownTransfer) {
		return
	}
	if err != nil {
		e.transferFailed(s, id, err)
		return
	}
	s.conn.Send(Message{Command: "file_status", TransferID: id, Status: "success"}, nil)
	if outcome.Tag == "" {
		peer := s.name
		e.log("received a file from " + peer)
		e.notify(func(d Delegate) { d.FileReceived(outcome.Path, peer) })
		return
	}
	if !bytes.HasPrefix(outcome.PNG, pngSignature) {
		e.log("clipboard image is not a PNG; dropped")
		return
	}
	e.apply(PNGContent(outcome.PNG), s)
}

func (e *Engine) transferFailed(s *session, id string, err error) {
	e.log("file transfer failed: " + err.Error())
	s.conn.Send(Message{Command: "error", TransferID: id, Message: "file transfer failed: " + err.Error()}, nil)
}

// MARK: - Files out

func (e *Engine) sendNextFile(s *session) {
	if s.sending != nil || s.conn.Closed || len(s.sendQueue) == 0 {
		return
	}
	path := s.sendQueue[0]
	s.sendQueue = s.sendQueue[1:]
	f := &outgoingFile{id: "file_" + randomHex(16), path: path}
	info, err := os.Stat(path)
	switch {
	case err != nil:
	case info.IsDir():
		err = errors.New("folders cannot be sent")
	case s.kind == Phone && info.Size() > MaxPhoneFileBytes:
		err = errors.New("it is larger than 512 MiB, more than the iPhone app can hold")
	default:
		f.size = info.Size()
		f.file, err = os.Open(path)
	}
	if err != nil {
		e.reportSend(f.name(), s.name, false, err.Error())
		e.sendNextFile(s)
		return
	}
	s.sending = f
	s.conn.Send(FileStart(f.name(), f.size, f.id, ""), nil)
	e.sendNextChunk(s, f)
	e.notifyState()
}

// sendNextChunk reads and sends one chunk, and the next when that one is out,
// so a large file is never in memory at once.
func (e *Engine) sendNextChunk(s *session, f *outgoingFile) {
	if s.sending != f {
		return
	}
	buf := make([]byte, fileChunkBytes)
	n, err := io.ReadFull(f.file, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		e.finishSend(s, f, false, "could not read it: "+err.Error())
		return
	}
	if f.sent+int64(n) > f.size {
		e.finishSend(s, f, false, "it changed while it was sent")
		return
	}
	if n == 0 {
		if f.sent != f.size {
			e.finishSend(s, f, false, "it changed while it was sent")
			return
		}
		s.conn.Send(FileEnd(f.id), func(ok bool) {
			switch {
			case !ok:
				e.finishSend(s, f, false, "the connection was lost")
			case s.kind == Phone:
				// The iPhone app sends no file_status.
				e.finishSend(s, f, true, "")
			default:
				e.queue.PostAfter(fileStatusTimeout, func() {
					e.finishSend(s, f, false, "the other computer did not confirm it")
				})
			}
		})
		return
	}
	f.sent += int64(n)
	index := f.chunks
	f.chunks++
	s.conn.Send(FileChunk(f.id, index, base64.StdEncoding.EncodeToString(buf[:n])), func(ok bool) {
		if !ok {
			e.finishSend(s, f, false, "the connection was lost")
			return
		}
		percent := 100
		if f.size > 0 {
			percent = int(f.sent * 100 / f.size)
		}
		if percent >= f.reportedPercent+5 {
			f.reportedPercent = percent
			e.notifyState()
		}
		e.sendNextChunk(s, f)
	})
}

func (e *Engine) finishSend(s *session, f *outgoingFile, ok bool, reason string) {
	if s.sending != f {
		return
	}
	s.sending = nil
	if f.file != nil {
		f.file.Close()
	}
	e.reportSend(f.name(), s.name, ok, reason)
	e.sendNextFile(s)
	e.notifyState()
}

func (e *Engine) reportSend(name, peer string, ok bool, reason string) {
	if ok {
		e.log("sent a file to " + peer)
	} else {
		e.log("could not send a file to " + peer + ": " + reason)
	}
	e.notify(func(d Delegate) { d.FileSent(name, peer, ok, reason) })
}

// sendImage sends a PNG as a file transfer tagged for the clipboard.
func (e *Engine) sendImage(png []byte, tag string, s *session) {
	id := "clip_" + randomHex(16)
	c := s.conn
	c.Send(FileStart("clipboard.png", int64(len(png)), id, tag), nil)
	offset, index := 0, 0
	// Chains the chunks so a 32 MiB image is not queued all at once.
	var next func()
	next = func() {
		if offset >= len(png) {
			c.Send(FileEnd(id), nil)
			return
		}
		end := min(offset+imageChunkBytes, len(png))
		chunk := base64.StdEncoding.EncodeToString(png[offset:end])
		offset = end
		i := index
		index++
		c.Send(FileChunk(id, i, chunk), func(ok bool) {
			if ok {
				next()
			}
		})
	}
	next()
}

// MARK: - Clipboard

// apply writes content from a peer locally and passes it on to every other
// session. Content seen last time stops here, which ends any loop through a
// ring of computers.
func (e *Engine) apply(content Content, source *session) {
	digest := content.Digest()
	if digest == e.lastSyncedDigest {
		return
	}
	e.lastSyncedDigest = digest
	e.ownChangeCount = e.clipboard.Write(content)
	e.haveOwnCount = true
	e.lastSeenCount = e.ownChangeCount
	e.broadcast(content, source)
}

func (e *Engine) broadcast(content Content, except *session) {
	if !content.Fits() {
		return
	}
	for _, s := range e.sessionList() {
		if s == except || s.state != paired || s.conn.Closed || s.kind == "" {
			continue
		}
		if !content.IsPNG {
			command := "clipboard_set"
			if s.kind == Phone {
				command = "clipboard_updated"
			}
			s.conn.Send(TextMessage(command, content.Text), nil)
		} else if s.peerFeatures["clipboard_image"] {
			tag := "set"
			if s.kind == Phone {
				tag = "updated"
			}
			e.sendImage(content.PNG, tag, s)
		}
	}
}

func (e *Engine) answerClipboardGet(s *session) {
	content, ok := e.clipboard.Read()
	switch {
	case ok && content.Fits() && !content.IsPNG:
		s.conn.Send(TextMessage("clipboard_content", content.Text), nil)
	case ok && content.Fits() && content.IsPNG && s.peerFeatures["clipboard_image"]:
		e.sendImage(content.PNG, "content", s)
	default:
		s.conn.Send(TextMessage("clipboard_content", ""), nil)
	}
}

// MARK: - Helpers

func (e *Engine) makeSnapshot() Snapshot {
	connected := map[string]bool{}
	for _, s := range e.sessions {
		if s.state == paired && !s.conn.Closed {
			connected[s.fp()] = true
		}
	}
	snap := Snapshot{Fingerprint: e.Identity.Fingerprint, Listening: e.listening}
	for fp, host := range e.knownHosts.All() {
		snap.Peers = append(snap.Peers, PeerState{Fingerprint: fp, Name: host.Name, Kind: host.Kind, Connected: connected[fp]})
	}
	sort.Slice(snap.Peers, func(i, j int) bool {
		return strings.ToLower(snap.Peers[i].Name) < strings.ToLower(snap.Peers[j].Name)
	})
	for fp, p := range e.pending {
		snap.Pending = append(snap.Pending, PairingRequest{Fingerprint: fp, Name: p.name, SuggestedKind: e.kindOf(fp)})
	}
	sort.Slice(snap.Pending, func(i, j int) bool { return snap.Pending[i].Name < snap.Pending[j].Name })
	for fp, d := range e.discovered {
		if _, known := e.knownHosts.Get(fp); d.IsComputer && !known {
			snap.Unpaired = append(snap.Unpaired, d)
		}
	}
	sort.Slice(snap.Unpaired, func(i, j int) bool { return snap.Unpaired[i].Name < snap.Unpaired[j].Name })
	for _, s := range e.sessions {
		if s.state == pairingOut {
			snap.Outgoing = append(snap.Outgoing, s.name)
		}
		if f := s.sending; f != nil {
			fraction := 1.0
			if f.size > 0 {
				fraction = float64(f.sent) / float64(f.size)
			}
			snap.Sending = append(snap.Sending, TransferState{Filename: f.name(), Peer: s.name, Fraction: fraction, Queued: len(s.sendQueue)})
		}
	}
	sort.Strings(snap.Outgoing)
	return snap
}

func (e *Engine) notify(f func(Delegate)) {
	if e.Delegate != nil {
		f(e.Delegate)
	}
}

func (e *Engine) notifyState() { e.notify(func(d Delegate) { d.StateChanged() }) }

func (e *Engine) fail(message string) {
	e.log(message)
	e.notify(func(d Delegate) { d.Failed(message) })
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
