package main

import (
	"testing"

	"github.com/freeyoung/tether-core-go/core"
)

func TestParseAddress(t *testing.T) {
	for _, c := range []struct {
		text string
		host string
		port int
		ok   bool
	}{
		{"", "", 0, false},
		{"fuji.local", "fuji.local", core.DefaultPort, true},
		{"192.0.2.4:6000", "192.0.2.4", 6000, true},
		{"[2001:db8::1]:6000", "2001:db8::1", 6000, true},
		{"[2001:db8::1]", "2001:db8::1", core.DefaultPort, true},
		{"2001:db8::1", "2001:db8::1", core.DefaultPort, true},
	} {
		host, port, ok := parseAddress(c.text)
		if host != c.host || port != c.port || ok != c.ok {
			t.Errorf("parseAddress(%q) = %q, %d, %v; want %q, %d, %v", c.text, host, port, ok, c.host, c.port, c.ok)
		}
	}
}

func TestUpdateSource(t *testing.T) {
	if url, ok := updateSource("0.2.0", ""); !ok || url != updateURL {
		t.Errorf("a release looks at %q, %v", url, ok)
	}
	if _, ok := updateSource("0.0.0-dev", ""); ok {
		t.Error("a build without a release version looks for updates")
	}
	if url, ok := updateSource("0.0.0-dev", "http://127.0.0.1/update.json"); !ok || url != "http://127.0.0.1/update.json" {
		t.Errorf("TETHER_UPDATE_URL gives %q, %v", url, ok)
	}
}

func TestShort(t *testing.T) {
	fp := "da17a42c00000000000000000000000000000000000000000000000000e62af492"
	if got := short(fp); got != "da17a42c…e62af492" {
		t.Errorf("short = %q", got)
	}
}
