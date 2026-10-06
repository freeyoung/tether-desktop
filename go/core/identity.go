package core

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"software.sslmate.com/src/go-pkcs12"
)

// SecretProtector protects the identity file at rest. The Windows app uses
// DPAPI; elsewhere the file's permissions are the protection.
type SecretProtector interface {
	Protect(data []byte) ([]byte, error)
	Unprotect(data []byte) ([]byte, error)
}

// FilePermissionsOnly keeps the bytes as they are.
type FilePermissionsOnly struct{}

func (FilePermissionsOnly) Protect(data []byte) ([]byte, error)   { return data, nil }
func (FilePermissionsOnly) Unprotect(data []byte) ([]byte, error) { return data, nil }

// Identity is the TLS identity this desktop presents: a self-signed RSA-2048
// certificate, like tetherd's and the iPhone app's.
type Identity struct {
	Certificate tls.Certificate
	// SHA-256 of the certificate's DER, lowercase hex, no separators.
	Fingerprint string
}

// FingerprintOf returns the SHA-256 of a certificate's DER, lowercase hex.
func FingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// The iPhone app and tetherd put a fixed name in their certificates. This
// engine uses the name of tetherd, so the name tells a phone from a computer.
const (
	phoneCertName    = "Tether-iOS"
	computerCertName = "Tether"
)

// KindFromCertName returns the kind that a certificate's name gives. It
// returns false for a name that neither the iPhone app nor tetherd uses.
func KindFromCertName(name string) (PeerKind, bool) {
	switch name {
	case phoneCertName:
		return Phone, true
	case computerCertName:
		return Computer, true
	}
	return "", false
}

// A PKCS#12 file with no password, on Windows.
const identityFile = "identity.pfx"

func LoadOrCreateIdentity(dir string, protector SecretProtector) (*Identity, error) {
	return LoadOrCreateIdentityFile(filepath.Join(dir, identityFile), "", protector)
}

// LoadOrCreateIdentityFile is LoadOrCreateIdentity for another file and
// password: the macOS app keeps its identity in identity.p12, with the
// password "tether". The file's permissions protect the key; the password
// only satisfies the PKCS#12 format.
func LoadOrCreateIdentityFile(path, password string, protector SecretProtector) (*Identity, error) {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		pfx, err := createIdentity(computerCertName, password)
		if err != nil {
			return nil, err
		}
		protected, err := protector.Protect(pfx)
		if err != nil {
			return nil, err
		}
		if err := WritePrivate(path, protected); err != nil {
			return nil, err
		}
	}
	protected, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pfx, err := protector.Unprotect(protected)
	if err != nil {
		return nil, fmt.Errorf("could not decrypt the identity: %w", err)
	}
	key, cert, _, err := pkcs12.DecodeChain(pfx, password)
	if err != nil {
		return nil, fmt.Errorf("could not read the identity: %w", err)
	}
	return &Identity{
		Certificate: tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert},
		Fingerprint: FingerprintOf(cert.Raw),
	}, nil
}

func createIdentity(commonName, password string) ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return pkcs12.Modern.Encode(key, cert, nil, password)
}

// FormatFingerprint shows a fingerprint as the iPhone app does, in
// colon-separated hex pairs, so the user can compare the 2 screens.
func FormatFingerprint(fp string, pairsPerLine int) string {
	var lines []string
	var pairs []string
	for i := 0; i+1 < len(fp); i += 2 {
		pairs = append(pairs, fp[i:i+2])
		if len(pairs) == pairsPerLine {
			lines = append(lines, strings.Join(pairs, ":"))
			pairs = nil
		}
	}
	if len(pairs) > 0 {
		lines = append(lines, strings.Join(pairs, ":"))
	}
	return strings.Join(lines, ":\n")
}

// EnsurePrivateDir creates the directory, readable by the owner only.
// %APPDATA% already belongs to the user alone on Windows.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// WritePrivate writes through a temporary file and a rename, readable by the
// owner only.
func WritePrivate(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
