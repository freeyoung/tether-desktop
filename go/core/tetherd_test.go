package core

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Against unmodified upstream tetherd in Docker (scripts/tetherd-container.sh up).
// Runs only when TETHERD_PORT and TETHERD_CONTAINER are set.
func TestPairingClipboardAndFilesWithTetherd(t *testing.T) {
	port, _ := strconv.Atoi(os.Getenv("TETHERD_PORT"))
	container := os.Getenv("TETHERD_CONTAINER")
	if port == 0 || container == "" {
		t.Skip("set TETHERD_PORT and TETHERD_CONTAINER to run against tetherd")
	}
	docker := func(args ...string) string {
		out, _ := exec.Command("docker", append([]string{"exec", container}, args...)...).CombinedOutput()
		return string(out)
	}
	n := newNode(t, "Test PC")
	n.engine.PairWithComputer("127.0.0.1", port)
	if !eventually(5*time.Second, func() bool {
		n.delegate.mu.Lock()
		defer n.delegate.mu.Unlock()
		for _, r := range n.delegate.outbound {
			if r.Kind == "waiting" && r.Peer != "" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("no pair_pending from tetherd")
	}
	docker("tether", "accept", n.engine.Identity.Fingerprint)
	if !eventually(5*time.Second, func() bool { return n.delegate.has("accepted") }) {
		t.Fatal("tetherd did not accept")
	}

	fromPC := "from the pc " + randomHex(4)
	n.clipboard.userCopies(TextContent(fromPC))
	n.engine.CheckLocalClipboard()
	if !eventually(5*time.Second, func() bool { return strings.Contains(docker("tether", "--get-clipboard"), fromPC) }) {
		t.Fatal("tetherd's clipboard did not change")
	}

	// A phone paired with tetherd: its copy reaches the PC through tetherd.
	p := newPhone(t)
	p.connect(port, true)
	p.send(PairRequest("Test iPhone"))
	p.expect("pair_pending")
	docker("tether", "accept", p.identity.Fingerprint)
	p.expect("pair_accepted")
	fromPhone := "from the phone " + randomHex(4)
	p.send(ClipboardSet(fromPhone))
	if !eventually(5*time.Second, func() bool { return n.clipboard.holds(TextContent(fromPhone)) }) {
		t.Fatal("tetherd did not forward")
	}

	// Files both ways.
	docker("sh", "-c", "printf 'line\\n%.0s' $(seq 2000) > /tmp/from-tetherd.txt")
	docker("tether", "--send-file", "/tmp/from-tetherd.txt")
	if !eventually(10*time.Second, func() bool { _, err := os.Stat(filepath.Join(n.downloads(), "from-tetherd.txt")); return err == nil }) {
		t.Fatal("the file did not arrive")
	}
	outgoing := filepath.Join(n.root, "to-tetherd.bin")
	os.WriteFile(outgoing, pattern(600_000), 0o644)
	block, _ := pem.Decode([]byte(docker("cat", "/data/.config/tether/cert.pem")))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	n.engine.SendFiles([]string{outgoing}, FingerprintOf(cert.Raw))
	if !eventually(10*time.Second, func() bool { s := n.delegate.sentList(); return len(s) == 1 && s[0].ok }) {
		t.Fatal("tetherd did not confirm", n.delegate.sentList())
	}
	if got := strings.TrimSpace(docker("sh", "-c", "wc -c < /downloads/to-tetherd.bin")); got != "600000" {
		t.Fatal(got)
	}
}
