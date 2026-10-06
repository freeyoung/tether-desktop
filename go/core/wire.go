// Package core is the Tether Desktop engine: the desktop side of the Tether
// protocol. It has no platform code, so its tests run on any OS.
package core

import (
	"bytes"
	"encoding/json"
)

// Message is one protocol message: a flat JSON object on one line. Only the
// fields of its command are present. The names match upstream
// TetherFramework's TetherMessage and tetherd. Fields that may be legitimately
// zero or empty are pointers, so that omitempty leaves out only absent ones.
type Message struct {
	Command    string  `json:"command"`
	Content    *string `json:"content,omitempty"`
	Filename   string  `json:"filename,omitempty"`
	Size       *int64  `json:"size,omitempty"`
	TransferID string  `json:"transfer_id,omitempty"`
	ChunkIndex *int    `json:"chunk_index,omitempty"`
	Data       string  `json:"data,omitempty"`
	// On file_start of a clipboard image: "set", "updated" or "content".
	Clipboard  string   `json:"clipboard,omitempty"`
	Features   []string `json:"features,omitempty"`
	DeviceName string   `json:"device_name,omitempty"`
	OTP        string   `json:"otp,omitempty"`
	Source     string   `json:"source,omitempty"`
	Status     string   `json:"status,omitempty"`
	Message    string   `json:"message,omitempty"`
}

func (m Message) content() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

func Hello(features ...string) Message         { return Message{Command: "hello", Features: features} }
func PairRequest(name string) Message          { return Message{Command: "pair_request", DeviceName: name} }
func PairAccepted() Message                    { return Message{Command: "pair_accepted"} }
func FileEnd(transferID string) Message        { return Message{Command: "file_end", TransferID: transferID} }
func TextMessage(command, text string) Message { return Message{Command: command, Content: &text} }

func FileStart(filename string, size int64, transferID, clipboard string) Message {
	return Message{Command: "file_start", Filename: filename, Size: &size, TransferID: transferID, Clipboard: clipboard}
}

func FileChunk(transferID string, index int, data string) Message {
	return Message{Command: "file_chunk", TransferID: transferID, ChunkIndex: &index, Data: data}
}

// Encode returns the message as one JSON line. Non-ASCII text stays as it is,
// like the other implementations.
func Encode(m Message) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(m) // Encode adds the newline.
	return buf.Bytes()
}

// Decode returns the message on a line, or false for a line that is not one,
// such as tetherd's bare "OK". Such lines are dropped and never answered:
// answering them loops forever.
func Decode(line []byte) (Message, bool) {
	var m Message
	if len(line) == 0 || json.Unmarshal(line, &m) != nil || m.Command == "" {
		return Message{}, false
	}
	return m, true
}

// MaxBufferedBytes: tetherd drops a session whose unparsed buffer passes
// 16 MiB; so do we.
const MaxBufferedBytes = 16 * 1024 * 1024

// LineBuffer collects bytes and hands out complete messages.
type LineBuffer struct {
	buf []byte
}

func (b *LineBuffer) Append(data []byte) { b.buf = append(b.buf, data...) }

func (b *LineBuffer) Len() int { return len(b.buf) }

func (b *LineBuffer) TakeMessages() []Message {
	var messages []Message
	start := 0
	for {
		i := bytes.IndexByte(b.buf[start:], '\n')
		if i < 0 {
			break
		}
		if m, ok := Decode(b.buf[start : start+i]); ok {
			messages = append(messages, m)
		}
		start += i + 1
	}
	if start > 0 {
		b.buf = append(b.buf[:0], b.buf[start:]...)
	}
	return messages
}
