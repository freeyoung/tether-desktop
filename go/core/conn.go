package core

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

// DefaultPort is the port tetherd and the iPhone app use.
const DefaultPort = 5134

type Direction string

const (
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

// keepAlive finds a peer that vanished within about a minute. tetherd had
// none before 0.2.36, which is how a dead session once hung it.
var keepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

// A write that makes no progress for this long means the peer is gone.
const writeTimeout = 60 * time.Second

// Peers are on the local network, so a TCP connect takes milliseconds. Just
// after a wake the network is often not up yet: a short timeout lets the
// redials try again soon. The TLS handshake has its own, longer limit.
const dialTimeout = 3 * time.Second

// Conn is one TLS session with a peer. I/O runs on its own goroutines; every
// callback is posted to the engine's queue, and the engine calls Send and
// Close only from that queue.
type Conn struct {
	ID          string
	Direction   Direction
	Fingerprint string
	// The common name in the peer's certificate: see KindFromCertName.
	CertName string
	// The resolved address of the other end, once connected.
	RemoteHost string
	RemotePort int
	Closed     bool

	OnReady   func(*Conn)
	OnMessage func(*Conn, Message)
	OnClose   func(*Conn, string)

	queue     *Queue
	identity  *Identity
	raw       net.Conn
	tls       *tls.Conn
	dialAddrs []string
	cancel    context.CancelFunc
	ctx       context.Context

	outMu   sync.Mutex
	outCond *sync.Cond
	out     []outgoing
	outDone bool
}

type outgoing struct {
	data []byte
	done func(bool)
}

func newConn(direction Direction, identity *Identity, queue *Queue) *Conn {
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{ID: hex.EncodeToString(id), Direction: direction, identity: identity, queue: queue, ctx: ctx, cancel: cancel}
	c.outCond = sync.NewCond(&c.outMu)
	return c
}

// Dial makes an outbound session to host:port.
func Dial(host string, port int, identity *Identity, queue *Queue) *Conn {
	return DialAny([]string{net.JoinHostPort(host, strconv.Itoa(port))}, identity, queue)
}

// DialAny tries the addresses (host:port) in order until 1 answers.
func DialAny(addrs []string, identity *Identity, queue *Queue) *Conn {
	c := newConn(Outbound, identity, queue)
	c.dialAddrs = addrs
	return c
}

// Accept wraps an inbound TCP connection.
func Accept(raw net.Conn, identity *Identity, queue *Queue) *Conn {
	c := newConn(Inbound, identity, queue)
	c.raw = raw
	return c
}

func (c *Conn) String() string {
	fp := "?"
	if len(c.Fingerprint) >= 12 {
		fp = c.Fingerprint[:12]
	}
	return string(c.Direction) + " " + fp + " [" + c.ID + "]"
}

func (c *Conn) Start() { go c.run() }

// Send queues a message. done runs on the queue when the bytes are out, or
// with false when they never will be.
func (c *Conn) Send(m Message, done func(bool)) {
	c.outMu.Lock()
	if c.Closed || c.outDone {
		c.outMu.Unlock()
		if done != nil {
			done(false)
		}
		return
	}
	c.out = append(c.out, outgoing{Encode(m), done})
	c.outCond.Signal()
	c.outMu.Unlock()
}

// Close runs on the queue. Closing sends a FIN, so the peer drops the session
// at once; tetherd before 0.2.36 has no keepalive and would keep it otherwise.
func (c *Conn) Close(reason string) {
	if c.Closed {
		return
	}
	c.Closed = true
	c.cancel()
	c.outMu.Lock()
	c.outDone = true
	pending := c.out
	c.out = nil
	c.outCond.Broadcast()
	c.outMu.Unlock()
	for _, item := range pending {
		if item.done != nil {
			done := item.done
			c.queue.Post(func() { done(false) })
		}
	}
	c.outMu.Lock()
	raw := c.raw
	c.outMu.Unlock()
	if raw != nil {
		// TLS close_notify may wait for the peer; never block the queue on it.
		go raw.Close()
	}
	onClose := c.OnClose
	c.OnReady, c.OnMessage, c.OnClose = nil, nil, nil
	if onClose != nil {
		onClose(c, reason)
	}
}

func (c *Conn) closeFromIO(reason string) { c.queue.Post(func() { c.Close(reason) }) }

// TLS 1.2 only: the iPhone app pins both its minimum and maximum to 1.2, and
// tetherd does the same. Certificates are self-signed, so any chain is
// accepted here and the engine pins fingerprints.
func (c *Conn) serverConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{c.identity.Certificate},
		// Without this the phone sends no certificate at all.
		ClientAuth: tls.RequireAnyClientCert,
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	}
}

func (c *Conn) clientConfig() *tls.Config {
	cert := c.identity.Certificate
	return &tls.Config{
		// Always offer our certificate, whatever issuers the server names.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil },
		InsecureSkipVerify:   true,
		ServerName:           "tether",
		MinVersion:           tls.VersionTLS12,
		MaxVersion:           tls.VersionTLS12,
	}
}

// dial connects to the first address that answers, each within
// dialTimeout, as clients with several addresses for a host do (RFC 8305
// tries them one after another too).
func (c *Conn) dial() (net.Conn, error) {
	dialer := net.Dialer{Timeout: dialTimeout, KeepAliveConfig: keepAlive}
	err := errors.New("no address to dial")
	for _, addr := range c.dialAddrs {
		var raw net.Conn
		if raw, err = dialer.DialContext(c.ctx, "tcp", addr); err == nil {
			return raw, nil
		}
		if c.ctx.Err() != nil {
			break
		}
	}
	return nil, err
}

func (c *Conn) run() {
	if c.Direction == Outbound {
		raw, err := c.dial()
		if err != nil {
			c.closeFromIO(err.Error())
			return
		}
		c.tls = tls.Client(raw, c.clientConfig())
	} else {
		if tcp, ok := c.raw.(*net.TCPConn); ok {
			_ = tcp.SetKeepAliveConfig(keepAlive)
		}
		c.tls = tls.Server(c.raw, c.serverConfig())
	}
	handshake, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	// Close reads raw on the queue: hand it over under the lock, or give up
	// if Close already ran.
	c.outMu.Lock()
	if c.outDone {
		c.outMu.Unlock()
		c.tls.Close()
		return
	}
	c.raw = c.tls
	c.outMu.Unlock()
	if err := c.tls.HandshakeContext(handshake); err != nil {
		if handshake.Err() == context.DeadlineExceeded {
			c.closeFromIO("handshake timed out")
		} else {
			c.closeFromIO(err.Error())
		}
		return
	}
	state := c.tls.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		c.closeFromIO("peer sent no certificate")
		return
	}
	fingerprint := FingerprintOf(state.PeerCertificates[0].Raw)
	certName := state.PeerCertificates[0].Subject.CommonName
	host, port := "", 0
	if addr, ok := c.tls.RemoteAddr().(*net.TCPAddr); ok {
		ip := addr.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		host, port = ip.String(), addr.Port
	}
	c.queue.Post(func() {
		if c.Closed {
			return
		}
		c.Fingerprint, c.CertName, c.RemoteHost, c.RemotePort = fingerprint, certName, host, port
		if c.OnReady != nil {
			c.OnReady(c)
		}
	})
	go c.writeLoop()
	c.readLoop()
}

func (c *Conn) readLoop() {
	var lines LineBuffer
	chunk := make([]byte, 256*1024)
	for {
		n, err := c.tls.Read(chunk)
		if n > 0 {
			lines.Append(chunk[:n])
			for _, m := range lines.TakeMessages() {
				m := m
				c.queue.Post(func() {
					if !c.Closed && c.OnMessage != nil {
						c.OnMessage(c, m)
					}
				})
			}
			if lines.Len() > MaxBufferedBytes {
				c.closeFromIO("line longer than 16 MiB")
				return
			}
		}
		if err != nil {
			if c.ctx.Err() == nil {
				c.closeFromIO("peer closed: " + err.Error())
			}
			return
		}
	}
}

func (c *Conn) writeLoop() {
	for {
		c.outMu.Lock()
		for len(c.out) == 0 && !c.outDone {
			c.outCond.Wait()
		}
		if c.outDone {
			c.outMu.Unlock()
			return
		}
		item := c.out[0]
		c.out = c.out[1:]
		c.outMu.Unlock()

		_ = c.tls.SetWriteDeadline(time.Now().Add(writeTimeout))
		_, err := c.tls.Write(item.data)
		if err != nil {
			if item.done != nil {
				done := item.done
				c.queue.Post(func() { done(false) })
			}
			c.closeFromIO("send failed: " + err.Error())
			return
		}
		if item.done != nil {
			done := item.done
			c.queue.Post(func() { done(true) })
		}
	}
}
