package core

import (
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

const tetherService = "_tether._tcp.local."

// Browser finds _tether._tcp services with multicast DNS. It asks from a
// random port, so responders answer it directly by unicast (RFC 6762, 6.7,
// "legacy unicast"). It never binds port 5353, which the system's own mDNS
// responder holds on Windows and macOS.
type Browser struct {
	Interval time.Duration
	Log      func(string)

	mu      sync.Mutex
	stop    chan struct{}
	entries map[string]*browseEntry
}

type browseEntry struct {
	// The name as the responder wrote it; map keys are lowercase.
	instance string
	target   string
	port     int
	txt      map[string]string
	addrs    map[string]net.IP
	seen     time.Time
}

func NewBrowser(log func(string)) *Browser {
	return &Browser{Interval: 10 * time.Second, Log: log, entries: map[string]*browseEntry{}}
}

func (b *Browser) Start(onResults func([]Discovered)) {
	b.mu.Lock()
	if b.stop != nil {
		b.mu.Unlock()
		return
	}
	b.stop = make(chan struct{})
	stop := b.stop
	b.mu.Unlock()
	go b.run(stop, onResults)
}

func (b *Browser) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
}

func (b *Browser) run(stop chan struct{}, onResults func([]Discovered)) {
	var last string
	// A few quick rounds first, so a new peer shows soon.
	delays := []time.Duration{0, time.Second, 3 * time.Second}
	for round := 0; ; round++ {
		delay := b.Interval
		if round < len(delays) {
			delay = delays[round]
		}
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
		b.round()
		results := b.results()
		key := resultsKey(results)
		if key != last {
			last = key
			onResults(results)
		}
	}
}

// round asks for the service, then for the records still missing.
func (b *Browser) round() {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		b.Log("mDNS: " + err.Error())
		return
	}
	defer conn.Close()
	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	sender := &multicastSender{conn: ipv4.NewPacketConn(conn), group: group, interfaces: lanInterfaces()}

	b.ask(sender, dns.Question{Name: tetherService, Qtype: dns.TypePTR, Qclass: dns.ClassINET})
	b.collect(conn, 1500*time.Millisecond)

	var missing []dns.Question
	b.mu.Lock()
	for name, e := range b.entries {
		if e.target == "" || e.txt == nil {
			missing = append(missing, dns.Question{Name: name, Qtype: dns.TypeSRV, Qclass: dns.ClassINET},
				dns.Question{Name: name, Qtype: dns.TypeTXT, Qclass: dns.ClassINET})
		} else if len(e.addrs) == 0 {
			missing = append(missing, dns.Question{Name: e.target, Qtype: dns.TypeA, Qclass: dns.ClassINET})
		}
	}
	b.mu.Unlock()
	if len(missing) > 0 {
		for _, q := range missing {
			b.ask(sender, q)
		}
		b.collect(conn, time.Second)
	}

	// Drop what did not answer for 3 rounds.
	b.mu.Lock()
	for name, e := range b.entries {
		if time.Since(e.seen) > 3*b.Interval+5*time.Second {
			delete(b.entries, name)
		}
	}
	b.mu.Unlock()
}

func (b *Browser) ask(sender *multicastSender, q dns.Question) {
	m := new(dns.Msg)
	m.Id = dns.Id()
	m.Question = []dns.Question{q}
	packed, err := m.Pack()
	if err != nil {
		b.Log("mDNS query: " + err.Error())
		return
	}
	sender.send(packed)
}

// multicastSender sends on every LAN interface. Without an interface the
// query follows the default route, which a VPN often owns.
type multicastSender struct {
	conn       *ipv4.PacketConn
	group      *net.UDPAddr
	interfaces []net.Interface
}

func (s *multicastSender) send(packet []byte) {
	for i := range s.interfaces {
		if s.conn.SetMulticastInterface(&s.interfaces[i]) == nil {
			_, _ = s.conn.WriteTo(packet, nil, s.group)
		}
	}
}

// lanInterfaces are the interfaces up with IPv4 and multicast, and not
// loopback or point-to-point (VPN tunnels).
func lanInterfaces() []net.Interface {
	all, _ := net.Interfaces()
	var lan []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 ||
			ifi.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				lan = append(lan, ifi)
				break
			}
		}
	}
	return lan
}

func (b *Browser) collect(conn *net.UDPConn, wait time.Duration) {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 9000)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil {
			continue
		}
		b.absorb(append(append(m.Answer, m.Extra...), m.Ns...))
	}
}

func (b *Browser) absorb(records []dns.RR) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := func(name string) *browseEntry {
		key := strings.ToLower(name)
		e, ok := b.entries[key]
		if !ok {
			e = &browseEntry{instance: name, addrs: map[string]net.IP{}}
			b.entries[key] = e
		}
		e.seen = time.Now()
		return e
	}
	// Instances first, so SRV and TXT that come before their PTR still land.
	for _, rr := range records {
		if ptr, ok := rr.(*dns.PTR); ok && strings.EqualFold(ptr.Hdr.Name, tetherService) {
			entry(ptr.Ptr)
		}
	}
	for _, rr := range records {
		switch r := rr.(type) {
		case *dns.SRV:
			if strings.HasSuffix(strings.ToLower(r.Hdr.Name), tetherService) {
				e := entry(r.Hdr.Name)
				e.target, e.port = strings.ToLower(r.Target), int(r.Port)
			}
		case *dns.TXT:
			if strings.HasSuffix(strings.ToLower(r.Hdr.Name), tetherService) {
				e := entry(r.Hdr.Name)
				e.txt = map[string]string{}
				for _, kv := range r.Txt {
					key, value, _ := strings.Cut(kv, "=")
					e.txt[strings.ToLower(key)] = value
				}
			}
		}
	}
	for _, rr := range records {
		if a, ok := rr.(*dns.A); ok {
			for _, e := range b.entries {
				if strings.EqualFold(e.target, a.Hdr.Name) {
					e.addrs[a.A.String()] = a.A
				}
			}
		}
	}
}

func (b *Browser) results() []Discovered {
	b.mu.Lock()
	defer b.mu.Unlock()
	var list []Discovered
	for _, e := range b.entries {
		fp := e.txt["fp"]
		if fp == "" || e.port == 0 || len(e.addrs) == 0 {
			continue
		}
		var hosts []string
		for addr := range e.addrs {
			hosts = append(hosts, addr)
		}
		sort.Strings(hosts)
		_, isComputer := e.txt["v"]
		list = append(list, Discovered{Name: instanceName(e.instance), Fingerprint: fp, Hosts: hosts, Port: e.port, IsComputer: isComputer})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Fingerprint < list[j].Fingerprint })
	return list
}

func resultsKey(list []Discovered) string {
	var parts []string
	for _, d := range list {
		parts = append(parts, d.Fingerprint+"|"+d.Name+"|"+strings.Join(d.Hosts, ",")+"|"+strconv.Itoa(d.Port)+"|"+strconv.FormatBool(d.IsComputer))
	}
	return strings.Join(parts, ";")
}

// instanceName turns "Alex\226\128\153s MacBook._tether._tcp.local." into
// "Alex’s MacBook": the first label, with DNS escapes undone.
func instanceName(fqdn string) string {
	labels := dns.SplitDomainName(fqdn)
	if len(labels) == 0 {
		return fqdn
	}
	label := labels[0]
	var out []byte
	for i := 0; i < len(label); i++ {
		if label[i] == '\\' && i+1 < len(label) {
			if i+3 < len(label) && isDigit(label[i+1]) && isDigit(label[i+2]) && isDigit(label[i+3]) {
				n, _ := strconv.Atoi(label[i+1 : i+4])
				out = append(out, byte(n))
				i += 3
				continue
			}
			out = append(out, label[i+1])
			i++
			continue
		}
		out = append(out, label[i])
	}
	return string(out)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
