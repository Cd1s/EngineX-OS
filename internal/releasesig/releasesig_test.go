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
