package core

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FileLimits caps what a peer may send.
type FileLimits struct {
	MaxFileBytes int64
	// The iPhone app and tetherd both cap clipboard images at 32 MiB.
	MaxClipboardBytes int64
	MaxOpenTransfers  int
}

var DefaultFileLimits = FileLimits{MaxFileBytes: 4 << 30, MaxClipboardBytes: 32 << 20, MaxOpenTransfers: 8}

// errUnknownTransfer: the transfer is gone, failed and reported already.
var errUnknownTransfer = errors.New("unknown transfer_id")

// FileOutcome is a finished transfer: a file in Downloads, or a clipboard
// image (Tag is the file_start "clipboard" value: set, updated or content).
type FileOutcome struct {
	Path string
	PNG  []byte
	Tag  string
}

type transferKey struct{ session, id string }

type transfer struct {
	filename  string
	size      int64
	clipboard string
	staging   string
	file      *os.File
	received  int64
}

// FileReceiver receives file_start / file_chunk / file_end. Data goes to a
// private staging directory first, and only a complete file is moved to
// Downloads.
type FileReceiver struct {
	staging   string
	downloads string
	limits    FileLimits
	// Marks a finished file the way a browser marks a download; on Windows,
	// with a Zone.Identifier stream.
	markDownloaded func(string)
	transfers      map[transferKey]*transfer
}

func NewFileReceiver(staging, downloads string, limits FileLimits, markDownloaded func(string)) *FileReceiver {
	// Leftovers from a crash are partial files: drop them.
	_ = os.RemoveAll(staging)
	return &FileReceiver{staging: staging, downloads: downloads, limits: limits, markDownloaded: markDownloaded,
		transfers: map[transferKey]*transfer{}}
}

func (r *FileReceiver) Start(session, id, filename string, size *int64, clipboard string) error {
	if id == "" || filename == "" || size == nil || *size < 0 {
		return errors.New("file_start without transfer_id, filename or size")
	}
	key := transferKey{session, id}
	if _, ok := r.transfers[key]; ok {
		return errors.New("duplicate transfer_id")
	}
	if len(r.transfers) >= r.limits.MaxOpenTransfers {
		return errors.New("too many open transfers")
	}
	limit := r.limits.MaxFileBytes
	if clipboard != "" {
		limit = r.limits.MaxClipboardBytes
	}
	if *size > limit {
		return errors.New("file is larger than the limit")
	}
	if err := EnsurePrivateDir(r.staging); err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	name := make([]byte, 16)
	_, _ = rand.Read(name)
	path := filepath.Join(r.staging, hex.EncodeToString(name))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("write failed: %w", err)
	}
	r.transfers[key] = &transfer{filename: Sanitize(filename), size: *size, clipboard: clipboard, staging: path, file: file}
	return nil
}

func (r *FileReceiver) Chunk(session, id, data string) error {
	key := transferKey{session, id}
	t, ok := r.transfers[key]
	if !ok {
		return errUnknownTransfer
	}
	bytes, err := base64.StdEncoding.DecodeString(data)
	if err != nil || data == "" {
		r.abort(key)
		return errors.New("chunk is not valid base64")
	}
	if t.received+int64(len(bytes)) > t.size {
		r.abort(key)
		return errors.New("more data than the declared size")
	}
	if _, err := t.file.Write(bytes); err != nil {
		r.abort(key)
		return fmt.Errorf("write failed: %w", err)
	}
	t.received += int64(len(bytes))
	return nil
}

func (r *FileReceiver) End(session, id string) (FileOutcome, error) {
	key := transferKey{session, id}
	t, ok := r.transfers[key]
	if !ok {
		return FileOutcome{}, errUnknownTransfer
	}
	delete(r.transfers, key)
	t.file.Close()
	defer os.Remove(t.staging)
	if t.received != t.size {
		return FileOutcome{}, errors.New("less data than the declared size")
	}
	if t.clipboard != "" {
		png, err := os.ReadFile(t.staging)
		if err != nil {
			return FileOutcome{}, fmt.Errorf("write failed: %w", err)
		}
		return FileOutcome{PNG: png, Tag: t.clipboard}, nil
	}
	if err := os.MkdirAll(r.downloads, 0o755); err != nil {
		return FileOutcome{}, fmt.Errorf("write failed: %w", err)
	}
	destination := UnusedPath(r.downloads, t.filename)
	if err := os.Rename(t.staging, destination); err != nil {
		return FileOutcome{}, fmt.Errorf("write failed: %w", err)
	}
	_ = os.Chmod(destination, 0o644)
	if r.markDownloaded != nil {
		r.markDownloaded(destination)
	}
	return FileOutcome{Path: destination}, nil
}

// CloseSession drops every open transfer of a session that closed.
func (r *FileReceiver) CloseSession(session string) {
	for key := range r.transfers {
		if key.session == session {
			r.abort(key)
		}
	}
}

func (r *FileReceiver) abort(key transferKey) {
	if t, ok := r.transfers[key]; ok {
		delete(r.transfers, key)
		t.file.Close()
		os.Remove(t.staging)
	}
}

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Sanitize keeps only the last path component, without characters Windows
// forbids, a leading dot, trailing dots or spaces, or a reserved device name,
// and at most 200 UTF-8 bytes. A name can never leave Downloads.
func Sanitize(raw string) string {
	parts := strings.FieldsFunc(strings.ReplaceAll(raw, "\\", "/"), func(r rune) bool { return r == '/' })
	last := ""
	if len(parts) > 0 {
		last = parts[len(parts)-1]
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"|?*`, r) {
			return -1
		}
		return r
	}, last)
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, ".")
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return "received-file"
	}
	ext := filepath.Ext(name)
	if reservedNames[strings.ToUpper(strings.TrimSuffix(name, ext))] {
		name = "_" + name
	}
	if len(name) > 200 {
		ext = filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		room := max(200-len(ext), 1)
		for len(stem) > room {
			_, size := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-size]
		}
		name = stem + ext
	}
	return name
}

// UnusedPath returns "photo.jpg", then "photo (1).jpg", "photo (2).jpg".
func UnusedPath(dir, name string) string {
	candidate := filepath.Join(dir, name)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 1; ; n++ {
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
	}
}
