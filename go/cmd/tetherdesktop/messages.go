package main

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"

	"github.com/freeyoung/tether-core-go/core"
)

// The Messages window shows the text messages of one phone that the engine
// keeps (Config.KeepMessages): messages.html in a web view. The submenu of
// the phone opens it. The page asks with JSON (handlePage), and the app
// answers with tether.receive(...). Only macOS has the window; elsewhere the
// engine keeps no messages.

//go:embed messages.html
var messagesPage string

// The apps that show text messages: a click on one of their notifications
// opens the window.
var smsApps = map[string]bool{
	"com.google.android.apps.messaging": true,
	"com.android.mms":                   true,
	"com.android.messaging":             true,
	"com.samsung.android.messaging":     true,
	"com.motorola.messaging":            true,
}

type messagesPageState struct {
	mu    sync.Mutex
	ready bool
	// The phone whose messages the window shows.
	peer string
	// The conversation to show once the page is ready.
	open string
}

func conversationKey(peer string, thread int64) string {
	b, _ := json.Marshal(thread)
	return peer + "|" + string(b)
}

// openMessages shows the window with the messages of a phone, at the
// conversation with this key if it is not "".
func (a *app) openMessages(peer, name, key string) {
	a.page.mu.Lock()
	ready, changed := a.page.ready, a.page.peer != peer
	a.page.peer = peer
	if !ready {
		a.page.open = key
	}
	a.page.mu.Unlock()
	showMessagesWindow(messagesPage, "Messages — "+name)
	if !ready {
		return
	}
	if changed {
		a.sendConversations()
	}
	if key != "" {
		a.toPage(map[string]any{"type": "open", "key": key})
	}
}

// openFromNotification: a click on a notification of a phone opens its
// conversation, or the window for a messages app.
func (a *app) openFromNotification(peer, who, app string) {
	store := a.engine.Messages()
	if store == nil {
		return
	}
	name, gives := peer[:8], false
	for _, p := range a.engine.SnapshotNow().Peers {
		if p.Fingerprint == peer {
			name, gives = p.Name, p.GivesMessages
		}
	}
	if !gives && !store.Has(peer) {
		return
	}
	if c, ok := store.Find(peer, who); ok {
		a.openMessages(peer, name, conversationKey(c.Peer, c.Thread))
	} else if smsApps[app] {
		a.openMessages(peer, name, "")
	}
}

func (a *app) toPage(v map[string]any) {
	data, _ := json.Marshal(v)
	evalInMessages("tether.receive(" + string(data) + ")")
}

// handlePage answers a request of the page.
func (a *app) handlePage(raw string) {
	store := a.engine.Messages()
	if store == nil {
		return
	}
	var r struct {
		Op     string `json:"op"`
		Peer   string `json:"peer"`
		Thread int64  `json:"thread"`
		Q      string `json:"q"`
		Text   string `json:"text"`
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return
	}
	switch r.Op {
	case "ready":
		a.page.mu.Lock()
		a.page.ready = true
		open := a.page.open
		a.page.open = ""
		a.page.mu.Unlock()
		a.sendConversations()
		if open != "" {
			a.toPage(map[string]any{"type": "open", "key": open})
		}
	case "thread":
		type message struct {
			core.SMS
			Code string `json:"code,omitempty"`
		}
		var list []message
		for _, m := range store.Thread(r.Peer, r.Thread) {
			code := ""
			if !m.Sent {
				code = core.FindCode(m.Body)
			}
			list = append(list, message{m, code})
		}
		a.toPage(map[string]any{"type": "thread", "key": conversationKey(r.Peer, r.Thread), "messages": list})
	case "search":
		a.toPage(map[string]any{"type": "search", "q": r.Q, "results": store.Search(a.peer(), r.Q, 200)})
	case "copy":
		if text := strings.TrimSpace(r.Text); text != "" {
			a.clipboard.Write(core.TextContent(text))
		}
	}
}

func (a *app) peer() string {
	a.page.mu.Lock()
	defer a.page.mu.Unlock()
	return a.page.peer
}

func (a *app) sendConversations() {
	a.toPage(map[string]any{"type": "conversations", "list": a.engine.Messages().Conversations(a.peer())})
}

// MessagesAdded: show the new messages if the window shows that phone, and
// refresh the menu: the first messages of a phone add Messages to its
// submenu.
func (a *app) MessagesAdded(peer string, _ []core.SMS) {
	a.page.mu.Lock()
	show := a.page.ready && a.page.peer == peer
	a.page.mu.Unlock()
	if show {
		go a.sendConversations()
	}
	a.requestRefresh()
}

// MessagesRequested: only phones answer it.
func (a *app) MessagesRequested(string, int64) {}
