package core

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestEncodeIsFlatAndKeepsZeroSizeAndText(t *testing.T) {
	got := string(Encode(FileStart("a.txt", 0, "T1", "")))
	want := `{"command":"file_start","filename":"a.txt","size":0,"transfer_id":"T1"}` + "\n"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if s := string(Encode(ClipboardSet("引吭 <&> 高歌"))); !strings.Contains(s, "引吭 <&> 高歌") {
		t.Fatalf("text escaped: %s", s)
	}
	if s := string(Encode(TextMessage("clipboard_content", ""))); !strings.Contains(s, `"content":""`) {
		t.Fatalf("empty content dropped: %s", s)
	}
}

func TestLineBufferDropsOKAndBrokenLines(t *testing.T) {
	var b LineBuffer
	b.Append([]byte("OK\n{\"command\":\"hello\",\"features\":[\"clipboard_image\"]}\nnot json\n{\"content\":\"x\"}\n{\"comm"))
	got := b.TakeMessages()
	if len(got) != 1 || got[0].Command != "hello" {
		t.Fatalf("got %+v", got)
	}
	b.Append([]byte("and\":\"clipboard_get\"}\n"))
	if got := b.TakeMessages(); len(got) != 1 || got[0].Command != "clipboard_get" || b.Len() != 0 {
		t.Fatalf("got %+v, %d left", got, b.Len())
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":      "passwd",
		`..\..\Windows\win.ini`: "win.ini",
		`C:\Users\x\photo.jpg`:  "photo.jpg",
		".bashrc":               "bashrc",
		"..":                    "received-file",
		"   ":                   "received-file",
		`a<b>c:d"e|f?g*h.txt`:   "abcdefgh.txt",
		"CON":                   "_CON",
		"nul.txt":               "_nul.txt",
		"report.pdf. . ":        "report.pdf",
		"a\x00b\nc.txt":         "abc.txt",
	}
	for in, want := range cases {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	long := Sanitize(strings.Repeat("é", 300) + ".pdf")
	if len(long) > 200 || !strings.HasSuffix(long, ".pdf") || !strings.HasPrefix(long, "é") {
		t.Errorf("long name: %d bytes", len(long))
	}
}

func TestUnusedPathCountsUp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "a (1).txt"), nil, 0o644)
	if got := filepath.Base(UnusedPath(dir, "a.txt")); got != "a (2).txt" {
		t.Fatal(got)
	}
}

func TestDigestAndLimits(t *testing.T) {
	if TextContent("hello\n").Digest() != TextContent("hello").Digest() {
		t.Error("trailing whitespace changed the digest")
	}
	if TextContent(" hello").Digest() == TextContent("hello").Digest() {
		t.Error("leading whitespace ignored")
	}
	if TextContent("x").Digest() == PNGContent([]byte("x")).Digest() {
		t.Error("text and image collide")
	}
	if !TextContent(strings.Repeat("a", MaxTextBytes)).Fits() || TextContent(strings.Repeat("a", MaxTextBytes+1)).Fits() {
		t.Error("text limit")
	}
	if PNGContent(make([]byte, MaxImageBytes+1)).Fits() {
		t.Error("image limit")
	}
}

func TestIdentityIsCreatedOnceAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	first, err := LoadOrCreateIdentity(dir, FilePermissionsOnly{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentity(dir, FilePermissionsOnly{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint != second.Fingerprint || len(first.Fingerprint) != 64 || strings.ToLower(first.Fingerprint) != first.Fingerprint {
		t.Fatalf("fingerprints %s %s", first.Fingerprint, second.Fingerprint)
	}
	if cn := first.Certificate.Leaf.Subject.CommonName; cn != "Tether" {
		t.Fatal(cn)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, "identity.pfx"))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", info.Mode())
		}
	}
	if names := listDir(t, dir); len(names) != 1 {
		t.Fatalf("left behind: %v", names)
	}
}

func TestFormatFingerprintMatchesThePhone(t *testing.T) {
	fp := "c520fd11138d23a7f98fa828fd4a7b5851c4325cfa1bf9517a85abd60d3ce729"
	text := FormatFingerprint(fp, 8)
	lines := strings.Split(text, "\n")
	if len(lines) != 4 || lines[0] != "c5:20:fd:11:13:8d:23:a7:" {
		t.Fatalf("%q", text)
	}
	var pairs []string
	for i := 0; i < 64; i += 2 {
		pairs = append(pairs, fp[i:i+2])
	}
	if strings.ReplaceAll(text, "\n", "") != strings.Join(pairs, ":") {
		t.Fatal("not the phone's format")
	}
}

func TestChooseWindows(t *testing.T) {
	const html, png, rtf, hint = 49300, 49400, 49500, 49600
	f := func(fs ...WindowsFormat) Choice { return ChooseWindows(fs) }
	cases := []struct {
		got, want Choice
		what      string
	}{
		{f(WindowsFormat{13, ""}, WindowsFormat{1, ""}), ChooseText, "text"},
		{f(WindowsFormat{png, "PNG"}, WindowsFormat{8, ""}), ChooseImage, "png"},
		{f(WindowsFormat{html, "HTML Format"}), ChooseSkip, "html only"},
		{f(), ChooseSkip, "empty"},
		{f(WindowsFormat{html, "HTML Format"}, WindowsFormat{png, "PNG"}, WindowsFormat{8, ""}, WindowsFormat{13, ""}), ChooseImage, "browser copy image"},
		{f(WindowsFormat{rtf, "Rich Text Format"}, WindowsFormat{13, ""}, WindowsFormat{png, "PNG"}), ChooseText, "word"},
		{f(WindowsFormat{15, ""}, WindowsFormat{13, ""}), ChooseSkip, "explorer files"},
		{f(WindowsFormat{13, ""}, WindowsFormat{hint, "ExcludeClipboardContentFromMonitorProcessing"}), ChooseSkip, "password manager"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.what, c.got, c.want)
		}
	}
}

func TestInstanceName(t *testing.T) {
	if got := instanceName(`Alex\226\128\153s MacBook._tether._tcp.local.`); got != "Alex’s MacBook" {
		t.Fatal(got)
	}
	if got := instanceName(`a\.b._tether._tcp.local.`); got != "a.b" {
		t.Fatal(got)
	}
}

func decodeB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func TestChooseMac(t *testing.T) {
	for _, c := range []struct {
		types []string
		want  Choice
	}{
		{[]string{"public.utf8-plain-text"}, ChooseText},
		{[]string{"public.png"}, ChooseImage},
		{[]string{"public.tiff"}, ChooseImage},
		{nil, ChooseSkip},
		{[]string{"com.adobe.pdf"}, ChooseSkip},
		// An editor: rich text, plain text, then a picture of it.
		{[]string{"public.rtf", "public.utf8-plain-text", "public.png"}, ChooseText},
		// A browser's "Copy Image": the image, then its address.
		{[]string{"public.tiff", "public.url", "public.utf8-plain-text"}, ChooseImage},
		// Finder file copies and secrets.
		{[]string{"public.file-url", "public.utf8-plain-text", "com.apple.icns"}, ChooseSkip},
		{[]string{"org.nspasteboard.ConcealedType", "public.utf8-plain-text"}, ChooseSkip},
		{[]string{"public.utf8-plain-text", "org.nspasteboard.TransientType"}, ChooseSkip},
	} {
		if got := ChooseMac(c.types); got != c.want {
			t.Errorf("%v: %v, want %v", c.types, got, c.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.2.0", "0.2.0", 0},
		{"v0.2.0", "0.2.0", 0},
		{"0.2.0", "0.10.0", -1},
		{"1.0.0", "0.9.9", 1},
		{"0.3.0-rc.1", "0.3.0", -1},
		{"0.3.0-rc.2", "0.3.0-rc.1", 1},
		{"0.0.0-dev", "0.0.0", -1},
	} {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestUpdateState(t *testing.T) {
	info := UpdateInfo{Latest: "0.4.0", MinSupported: "0.3.0"}
	for current, want := range map[string]UpdateState{
		"0.4.0": UpToDate, "0.5.0": UpToDate, "0.3.1": UpdateAvailable, "0.2.9": UpdateRequired,
	} {
		if got := info.State(current); got != want {
			t.Errorf("%s: %v, want %v", current, got, want)
		}
	}
}

func TestFetchUpdateInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"latest": "0.4.0", "min_supported": "0.3.0"}`)
	}))
	defer server.Close()
	info, err := FetchUpdateInfo(context.Background(), server.URL)
	if err != nil || info != (UpdateInfo{Latest: "0.4.0", MinSupported: "0.3.0"}) {
		t.Fatal(info, err)
	}
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	if _, err := FetchUpdateInfo(context.Background(), missing.URL); err == nil {
		t.Fatal("no error for a 404")
	}
}
