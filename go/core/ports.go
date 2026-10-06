package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Content is what is on a clipboard: text, or a PNG image.
type Content struct {
	Text  string
	PNG   []byte
	IsPNG bool
}

func TextContent(text string) Content { return Content{Text: text} }
func PNGContent(png []byte) Content   { return Content{PNG: png, IsPNG: true} }

func (c Content) Equal(o Content) bool {
	return c.IsPNG == o.IsPNG && c.Text == o.Text && bytes.Equal(c.PNG, o.PNG)
}

const (
	// tetherd drops a session whose line passes 16 MiB, and JSON escaping can
	// grow text several times over, so text stays well below that.
	MaxTextBytes = 4 << 20
	// The iPhone app and tetherd both reject clipboard images above 32 MiB.
	MaxImageBytes = 32 << 20
)

// Fits tells whether the content may go to peers at all.
func (c Content) Fits() bool {
	if c.IsPNG {
		return len(c.PNG) <= MaxImageBytes
	}
	return len(c.Text) <= MaxTextBytes
}

// Digest identifies the content for loop prevention. Trailing whitespace is
// ignored because tetherd trims it, so the same text can come back changed
// only there.
func (c Content) Digest() string {
	h := sha256.New()
	if c.IsPNG {
		h.Write([]byte("p"))
		h.Write(c.PNG)
	} else {
		h.Write([]byte("t"))
		h.Write([]byte(strings.TrimRightFunc(c.Text, isSpace)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0xA0 ||
		(r >= 0x2000 && r <= 0x200A) || r == 0x1680 || r == 0x2028 || r == 0x2029 || r == 0x202F || r == 0x205F || r == 0x3000
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// Clipboard is the local clipboard. The engine calls it on its queue.
type Clipboard interface {
	// ChangeCount changes whenever anyone writes the clipboard.
	ChangeCount() uint64
	// Read returns what the user copied, or false for nothing that goes to peers.
	Read() (Content, bool)
	// Write returns the change count after the write.
	Write(Content) uint64
}

// Discovered is a desktop or phone that publishes _tether._tcp. tetherd and
// Tether Desktop publish v=1 in the TXT record; the phone does not.
type Discovered struct {
	Name        string
	Fingerprint string
	// Every address the service has: a computer may be on Ethernet and
	// Wi-Fi at once, and only some of its addresses may answer.
	Hosts      []string
	Port       int
	IsComputer bool
}

// Discovery advertises this desktop and finds others. Results may come on any
// goroutine; the engine posts them to its queue.
type Discovery interface {
	Start(instance string, port int, fingerprint string, onResults func([]Discovered))
	Stop()
}

type PairingRequest struct {
	Fingerprint string
	Name        string
	// What the engine finds the peer to be: see Engine.kindOf.
	SuggestedKind PeerKind
}

// OutboundResult is how a pairing request to a computer went.
type OutboundResult struct {
	Kind   string // "waiting", "accepted", "rejected", "failed"
	Peer   string
	Reason string
}

// Delegate is the app side. The engine calls it on its queue; the app
// marshals to its UI thread where needed.
type Delegate interface {
	PairingRequested(PairingRequest)
	PairingResolved(fingerprint string)
	OutboundPairing(OutboundResult)
	OpenURL(url string)
	FileReceived(path, peer string)
	FileSent(name, peer string, ok bool, reason string)
	Failed(message string)
	StateChanged()
	// PeerOutdated: a paired Tether Desktop runs a version older than
	// MinPeerVersion, so its session was closed.
	PeerOutdated(name, version string)
}

type PeerState struct {
	Fingerprint string
	Name        string
	Kind        PeerKind
	Connected   bool
}

type TransferState struct {
	Filename string
	Peer     string
	Fraction float64
	// Files waiting behind this one for the same peer.
	Queued int
}

type Snapshot struct {
	Fingerprint string
	Listening   bool
	Peers       []PeerState
	Pending     []PairingRequest
	// Computers on the network that are not paired yet.
	Unpaired []Discovered
	// Computers this desktop has asked to pair, still waiting.
	Outgoing []string
	Sending  []TransferState
}

// WindowsFormat is one Windows clipboard format, in the order
// EnumClipboardFormats gives them.
type WindowsFormat struct {
	ID   uint32
	Name string
}

const (
	CF_TEXT        = 1
	CF_BITMAP      = 2
	CF_OEMTEXT     = 7
	CF_DIB         = 8
	CF_UNICODETEXT = 13
	CF_HDROP       = 15
	CF_DIBV5       = 17
)

type Choice int

const (
	ChooseSkip Choice = iota
	ChooseText
	ChooseImage
)

// Password managers mark secrets with these. The app passes
// CanIncludeInClipboardHistory only when its value is 0; some apps set it to
// 1, which allows the content.
var privateFormats = map[string]bool{
	"excludeclipboardcontentfrommonitorprocessing": true,
	"canincludeinclipboardhistory":                 true,
	"clipboard viewer ignore":                      true,
}

// Pasteboard types on macOS. Password managers mark secrets with the first
// 3 (nspasteboard.org). Finder puts the file names as text next to the
// files; that text is not what the user copied.
var (
	macPrivateTypes = map[string]bool{
		"org.nspasteboard.ConcealedType":     true,
		"org.nspasteboard.TransientType":     true,
		"org.nspasteboard.AutoGeneratedType": true,
		"public.file-url":                    true,
		"NSFilenamesPboardType":              true,
	}
	macTextTypes  = map[string]bool{"public.utf8-plain-text": true, "public.plain-text": true, "NSStringPboardType": true}
	macImageTypes = map[string]bool{"public.png": true, "public.tiff": true, "public.jpeg": true, "public.heic": true}
)

// ChooseMac decides what of the macOS pasteboard goes to peers, from its
// types in the order the owner wrote them, most faithful first. A browser's
// "Copy Image" writes the image first and its address after; an editor
// writes the text first and a picture of it after.
func ChooseMac(types []string) Choice {
	text, image := -1, -1
	for i, t := range types {
		if macPrivateTypes[t] {
			return ChooseSkip
		}
		switch {
		case text < 0 && macTextTypes[t]:
			text = i
		case image < 0 && macImageTypes[t]:
			image = i
		}
	}
	switch {
	case text < 0 && image < 0:
		return ChooseSkip
	case text < 0:
		return ChooseImage
	case image < 0:
		return ChooseText
	case image < text:
		return ChooseImage
	}
	return ChooseText
}

// ChooseWindows decides what of the Windows clipboard goes to peers: skip
// secrets and Explorer file copies, and pick text or image by the order the
// owner set them, as ChooseMac does.
func ChooseWindows(formats []WindowsFormat) Choice {
	text, image := -1, -1
	for i, f := range formats {
		if f.ID == CF_HDROP || privateFormats[strings.ToLower(f.Name)] {
			return ChooseSkip
		}
		switch {
		case text < 0 && (f.ID == CF_UNICODETEXT || f.ID == CF_TEXT || f.ID == CF_OEMTEXT):
			text = i
		case image < 0 && (f.ID == CF_DIBV5 || f.ID == CF_DIB || f.ID == CF_BITMAP || strings.EqualFold(f.Name, "PNG")):
			image = i
		}
	}
	switch {
	case text < 0 && image < 0:
		return ChooseSkip
	case text < 0:
		return ChooseImage
	case image < 0:
		return ChooseText
	// A browser's "Copy image" sets the image first; an editor sets the text
	// first and a picture of it after.
	case image < text:
		return ChooseImage
	default:
		return ChooseText
	}
}

// truncate shortens a peer-supplied name to 100 characters.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
