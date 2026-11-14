package releasesig

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return pub, priv, pubPEM, privPEM
}

func TestSignVerifyAndRotation(t *testing.T) {
	pub1, priv1, pub1PEM, priv1PEM := testKeyPair(t)
	pub2, priv2, pub2PEM, _ := testKeyPair(t)
	_, privWrong, _, _ := testKeyPair(t)

	parsedPriv, err := ParsePrivateKeyPEM([]byte(priv1PEM))
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM: %v", err)
	}
	if !parsedPriv.Equal(priv1) {
		t.Fatal("parsed private key does not match generated key")
	}

	keys, err := ParsePublicKeysPEM([]string{pub1PEM, pub2PEM})
	if err != nil {
		t.Fatalf("ParsePublicKeysPEM: %v", err)
	}
	if len(keys) != 2 || !keys[0].Equal(pub1) || !keys[1].Equal(pub2) {
		t.Fatalf("parsed keys mismatch: %v", keys)
	}

	sums := []byte(strings.Repeat("a", 64) + "  SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img\n")
	sig1, err := Sign(priv1, sums)
	if err != nil {
		t.Fatalf("Sign key1: %v", err)
	}
	sig2, err := Sign(priv2, sums)
	if err != nil {
		t.Fatalf("Sign key2: %v", err)
	}
	sigWrong, err := Sign(privWrong, sums)
	if err != nil {
		t.Fatalf("Sign wrong key: %v", err)
	}

	// Both current and rotated keys verify against the key list.
	for name, sig := range map[string][]byte{"key1": sig1, "key2": sig2} {
		digest, err := VerifyAsset(sums, sig, "SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", keys)
		if err != nil || digest != strings.Repeat("a", 64) {
			t.Fatalf("%s VerifyAsset = (%q, %v)", name, digest, err)
		}
	}

	// Untrusted key is rejected.
	if _, err := VerifyAsset(sums, sigWrong, "SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", keys); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("wrong key err = %v, want ErrInvalidSignature", err)
	}

	// Empty key list fails closed.
	if err := VerifySignature(sums, sig1, nil); !errors.Is(err, ErrNoTrustedKeys) {
		t.Fatalf("empty keys err = %v, want ErrNoTrustedKeys", err)
	}

	// Tampered SHA256SUMS.txt is rejected.
	tamperedSums := append([]byte(nil), sums...)
	tamperedSums[0] = 'b'
	if err := VerifySignature(tamperedSums, sig1, keys); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered sums err = %v, want ErrInvalidSignature", err)
	}

	// Tampered or malformed signature is rejected.
	rawSig, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig1)))
	rawSig[0] ^= 0xff
	tamperedSig := []byte(base64.StdEncoding.EncodeToString(rawSig) + "\n")
	if err := VerifySignature(sums, tamperedSig, keys); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered sig err = %v, want ErrInvalidSignature", err)
	}
	if err := VerifySignature(sums, []byte("not-base64!"), keys); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("malformed sig err = %v, want ErrInvalidSignature", err)
	}

	// Missing asset in signed SHA256SUMS.txt is rejected.
	if _, err := VerifyAsset(sums, sig1, "OTHER-5.3.0.5-Update.img", keys); err == nil {
		t.Fatal("missing asset accepted")
	}
}

func TestLoadPrivateKeyFilePermissionsAndFormat(t *testing.T) {
	_, _, _, privPEM := testKeyPair(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "release.key")
	if err := os.WriteFile(keyPath, []byte(privPEM), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKeyFile(keyPath); err == nil {
		t.Fatal("0644 private key file must be refused")
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPrivateKeyFile(keyPath); err != nil {
		t.Fatalf("0600 private key file rejected: %v", err)
	}

	// Non-Ed25519 PKCS#8 key must be rejected without leaking key bytes.
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecPriv)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})
	if _, err := ParsePrivateKeyPEM(ecPEM); err == nil || strings.Contains(err.Error(), "BEGIN") {
		t.Fatalf("expected non-Ed25519 error without PEM leakage, got %v", err)
	}
}

func TestParseSHA256SUMSValidation(t *testing.T) {
	if _, err := ParseSHA256SUMS([]byte("")); err == nil {
		t.Fatal("empty SHA256SUMS accepted")
	}
	badPath := strings.Repeat("a", 64) + "  ../evil.img\n"
	if _, err := ParseSHA256SUMS([]byte(badPath)); err == nil {
		t.Fatal("traversal path accepted")
	}
	conflict := strings.Repeat("a", 64) + "  a.img\n" + strings.Repeat("b", 64) + "  a.img\n"
	if _, err := ParseSHA256SUMS([]byte(conflict)); err == nil {
		t.Fatal("conflicting duplicate entries accepted")
	}
}

func TestEmbeddedTrustedPublicKeysValid(t *testing.T) {
	if _, err := TrustedPublicKeys(); err != nil {
		t.Fatalf("embedded TrustedPublicKeysPEM invalid: %v", err)
	}
}

func TestEmbeddedReleaseKeyIsTheMaintainersKey(t *testing.T) {
	keys, err := TrustedPublicKeys()
	if err != nil || len(keys) != 1 {
		t.Fatalf("embedded keys = %d, %v; want the one release key", len(keys), err)
	}
	der, err := x509.MarshalPKIXPublicKey(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(der); got != "MCowBQYDK2VwAyEAkVGt4Q1qVUQV5/PW5Il8lxuTjMIpW+RaNBt3RSJWJk0=" {
		t.Fatalf("embedded key is %s", got)
	}
	// The key is found in built binaries by its one base64 line (release-gate.sh): keep the PEM in that shape.
	if !strings.Contains(TrustedPublicKeysPEM[0], "\nMCowBQYDK2VwAyEAkVGt4Q1qVUQV5/PW5Il8lxuTjMIpW+RaNBt3RSJWJk0=\n") {
		t.Fatal("the embedded PEM does not carry the key as one base64 line")
	}
}

// The file names of a release carry the version but not the -rN revision; the tag entry is what
// binds a genuine signed list to one release tag (a genuine r1 shown as r3 must not verify).
func TestTagBinding(t *testing.T) {
	list := func(tag string) []byte {
		return []byte(strings.Repeat("a", 64) + "  SC5000-5.3.0.5-EngineX-OS-x-Update.img\n" + TagDigest(tag) + "  " + TagEntryName + "\n")
	}
	if err := VerifyTag(list("v5.3.0.5-r2"), "v5.3.0.5-r2"); err != nil {
		t.Fatalf("the signed tag was refused: %v", err)
	}
	for _, c := range []struct {
		name, tag string
		sums      []byte
	}{
		{"r1 shown as r3", "v5.3.0.5-r3", list("v5.3.0.5-r1")},
		{"r2 shown as the plain release", "v5.3.0.5", list("v5.3.0.5-r2")},
		{"no tag entry", "v5.3.0.5", []byte(strings.Repeat("a", 64) + "  SC5000-5.3.0.5-EngineX-OS-x-Update.img\n")},
		{"empty tag", "", list("")},
		{"tag with a line break", "v5.3.0.5\nx", list("v5.3.0.5\nx")},
	} {
		if err := VerifyTag(c.sums, c.tag); !errors.Is(err, ErrTagMismatch) {
			t.Errorf("%s: err = %v, want ErrTagMismatch", c.name, err)
		}
	}
	// The entry is the hash of the RELEASE-TAG.txt file the build writes: the tag and a newline
	// (printf 'v5.3.0.5\n' | sha256sum).
	if got := TagDigest("v5.3.0.5"); got != "369bd4c0f3141cf1d1d0bee66890a06ffad4d8636971f5d303c240395948352e" {
		t.Fatalf("TagDigest = %s", got)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v2", "v1", true},
		{"v1", "v1", false},
		{"v1", "v2", false}, // rollback / lower latest
		{"v5.0.4-custom.10", "v5.0.4-custom.9", true},
		{"v5.0.4-custom.1", "v5.0.4", true},
		{"v5.0.4", "dev", false}, // unknown current version
		{"", "v1", false},
	} {
		if got := NewerVersion(c.latest, c.current); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v", c.latest, c.current, got)
		}
	}
}

// The deck and the installer decide "older" with the same functions: the release a deck runs is the
// newest tag of its installed version, else the version.
func TestInstalledRelease(t *testing.T) {
	for _, c := range []struct {
		name, version string
		tags          []string
		want          string
	}{
		{"no tag", "5.3.0.5", nil, "5.3.0.5"},
		{"a re-release", "5.3.0.5", []string{"v5.3.0.5-r2"}, "v5.3.0.5-r2"},
		{"the newest of several", "5.3.0.5", []string{"v5.3.0.5-r2", "v5.3.0.5-r10", "v5.3.0.5"}, "v5.3.0.5-r10"},
		{"a tag left by another version is ignored", "5.3.0.4", []string{"v5.3.0.5-r2"}, "5.3.0.4"},
		{"not a tag at all", "5.3.0.5", []string{"dev", ""}, "5.3.0.5"},
	} {
		if got := InstalledRelease(c.version, c.tags); got != c.want {
			t.Errorf("%s: InstalledRelease = %q, want %q", c.name, got, c.want)
		}
	}
	// What the installer refuses: a release older than the one the deck runs; the same one is allowed.
	for _, c := range []struct {
		installed, offered string
		older              bool
	}{
		{"v5.3.0.5-r2", "v5.3.0.5-r1", true},
		{"v5.3.0.5-r2", "v5.3.0.5", true},
		{"v5.3.0.5-r2", "v5.3.0.4", true},
		{"v5.3.0.5-r2", "v5.3.0.5-r2", false},
		{"v5.3.0.5-r2", "v5.3.0.5-r3", false},
		{"v5.3.0.5-r2", "v5.3.0.6", false},
		{"dev", "v5.3.0.5", false}, // nothing known about the deck: not a reason to refuse
	} {
		if got := NewerVersion(c.installed, c.offered); got != c.older {
			t.Errorf("NewerVersion(%q, %q) = %v, want %v", c.installed, c.offered, got, c.older)
		}
	}
}
