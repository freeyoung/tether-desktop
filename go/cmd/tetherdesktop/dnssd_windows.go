package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/freeyoung/tether-core-go"
)

var (
	dnsapi                       = windows.NewLazySystemDLL("dnsapi.dll")
	pDnsServiceConstructInstance = dnsapi.NewProc("DnsServiceConstructInstance")
	pDnsServiceRegister          = dnsapi.NewProc("DnsServiceRegister")
	pDnsServiceDeRegister        = dnsapi.NewProc("DnsServiceDeRegister")
)

const (
	dnsQueryRequestVersion1 = 1
	dnsRequestPending       = 9506
)

type dnsServiceRegisterRequest struct {
	version         uint32
	interfaceIndex  uint32
	serviceInstance uintptr
	completion      uintptr
	queryContext    uintptr
	credentials     uintptr
	unicastEnabled  int32
}

// discovery advertises _tether._tcp with the system's DNS-SD service, so
// Windows' own mDNS responder answers and no extra firewall rule is needed,
// and browses with core.Browser.
type discovery struct {
	browser *core.Browser
	request *dnsServiceRegisterRequest
	// The system keeps these and calls back later: keep them alive.
	keys, values []*uint16
	callback     uintptr
	// The system calls the same callback when the record goes away.
	stopping atomic.Bool
}

func newDiscovery() *discovery {
	return &discovery{browser: core.NewBrowser(logLine)}
}

func (d *discovery) Start(instance string, port int, fingerprint string, onResults func([]core.Discovered)) {
	d.register(instance, port, fingerprint)
	d.browser.Start(onResults)
}

func (d *discovery) Stop() {
	d.browser.Stop()
	d.stopping.Store(true)
	if d.request != nil {
		pDnsServiceDeRegister.Call(uintptr(unsafe.Pointer(d.request)), 0)
	}
}

func (d *discovery) register(instance string, port int, fingerprint string) {
	if err := pDnsServiceRegister.Find(); err != nil {
		logLine("DNS-SD registration is not available: " + err.Error())
		return
	}
	host, _ := os.Hostname()
	// A dot would split the instance name into labels.
	name, _ := windows.UTF16PtrFromString(strings.ReplaceAll(instance, ".", "-") + "._tether._tcp.local")
	hostName, _ := windows.UTF16PtrFromString(host + ".local")
	// tetherd publishes exactly fp and v; the phone reads fp.
	d.keys = []*uint16{wstr("fp"), wstr("v")}
	d.values = []*uint16{wstr(fingerprint), wstr("1")}
	record, _, _ := pDnsServiceConstructInstance.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(hostName)), 0, 0,
		uintptr(port), 0, 0, 2, uintptr(unsafe.Pointer(&d.keys[0])), uintptr(unsafe.Pointer(&d.values[0])))
	if record == 0 {
		logLine("could not build the DNS-SD record")
		return
	}
	d.callback = windows.NewCallback(func(status uint32, _ uintptr, _ uintptr) uintptr {
		switch {
		case d.stopping.Load():
			logLine("stopped advertising _tether._tcp")
		case status == 0:
			logLine("advertised _tether._tcp")
		default:
			logLine(fmt.Sprintf("DNS-SD registration failed: %d", status))
		}
		return 0
	})
	d.request = &dnsServiceRegisterRequest{version: dnsQueryRequestVersion1, serviceInstance: record, completion: d.callback}
	if status, _, _ := pDnsServiceRegister.Call(uintptr(unsafe.Pointer(d.request)), 0); status != dnsRequestPending {
		logLine(fmt.Sprintf("DNS-SD registration failed at once: %d", status))
	}
}

func wstr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}
