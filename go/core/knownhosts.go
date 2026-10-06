package core

import (
	"encoding/json"
	"os"
)

// PeerKind is what a pinned peer is. The protocol has no device type, so the
// engine takes it from the name in the peer's certificate (see
// KindFromCertName) and records it at pairing time. A phone applies
// clipboard_updated; a computer (tetherd or another Tether Desktop) applies
// clipboard_set.
type PeerKind string

const (
	Phone    PeerKind = "phone"
	Computer PeerKind = "computer"
)

// KnownHost is one pinned peer.
type KnownHost struct {
	Name string   `json:"name"`
	Kind PeerKind `json:"kind"`
	// The last address that worked, for computers that discovery misses.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
}

// KnownHosts are the pinned peers, keyed by fingerprint, in
// known_hosts.json.
type KnownHosts struct {
	path  string
	hosts map[string]KnownHost
	log   func(string)
}

type knownHostsFile struct {
	Version int                  `json:"version"`
	Hosts   map[string]KnownHost `json:"hosts"`
}

func LoadKnownHosts(path string, log func(string)) *KnownHosts {
	k := &KnownHosts{path: path, hosts: map[string]KnownHost{}, log: log}
	data, err := os.ReadFile(path)
	if err != nil {
		return k
	}
	var file knownHostsFile
	if err := json.Unmarshal(data, &file); err != nil {
		log("known_hosts.json is unreadable, starting empty: " + err.Error())
		return k
	}
	for fp, host := range file.Hosts {
		k.hosts[fp] = host
	}
	return k
}

func (k *KnownHosts) Get(fp string) (KnownHost, bool) {
	host, ok := k.hosts[fp]
	return host, ok
}

func (k *KnownHosts) All() map[string]KnownHost {
	all := make(map[string]KnownHost, len(k.hosts))
	for fp, host := range k.hosts {
		all[fp] = host
	}
	return all
}

func (k *KnownHosts) Set(fp string, host KnownHost) {
	k.hosts[fp] = host
	k.save()
}

func (k *KnownHosts) Remove(fp string) bool {
	if _, ok := k.hosts[fp]; !ok {
		return false
	}
	delete(k.hosts, fp)
	k.save()
	return true
}

func (k *KnownHosts) save() {
	data, err := json.MarshalIndent(knownHostsFile{Version: 1, Hosts: k.hosts}, "", "  ")
	if err == nil {
		err = WritePrivate(k.path, data)
	}
	if err != nil {
		k.log("could not save known_hosts.json: " + err.Error())
	}
}
