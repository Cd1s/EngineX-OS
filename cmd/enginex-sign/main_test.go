package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestKeys(t *testing.T, dir string) (pubPath, privPath string) {
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
	privPath = filepath.Join(dir, "release.key")
	pubPath = filepath.Join(dir, "release.pub")
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		t.Fatal(err)
	}
	return pubPath, privPath
}

func TestSignAndVerifyTool(t *testing.T) {
	dir := t.TempDir()
	pubPath, privPath := writeTestKeys(t, dir)
	sumsPath := filepath.Join(dir, "SHA256SUMS.txt")
	sumsContent := strings.Repeat("0", 64) + "  SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img\n"
	if err := os.WriteFile(sumsPath, []byte(sumsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Missing ENGINEX_RELEASE_KEY fails cleanly.
	var out, errBuf bytes.Buffer
	if err := run([]string{sumsPath}, func(string) string { return "" }, &out, &errBuf); err == nil {
		t.Fatal("expected error when ENGINEX_RELEASE_KEY is unset")
	}

	// Signing writes SHA256SUMS.txt.sig and prints no key material.
	out.Reset()
	errBuf.Reset()
	getenv := func(k string) string {
		if k == keyEnvVar {
			return privPath
		}
		return ""
	}
	if err := run([]string{sumsPath}, getenv, &out, &errBuf); err != nil {
		t.Fatalf("sign failed: %v", err)
	}
	combined := out.String() + errBuf.String()
	if strings.Contains(combined, "PRIVATE KEY") || strings.Contains(combined, "BEGIN") {
		t.Fatalf("output leaked key material: %q", combined)
	}

	// --verify succeeds with matching --pubkey.
	out.Reset()
	if err := run([]string{"--verify", "--pubkey", pubPath, sumsPath}, getenv, &out, &errBuf); err != nil {
		t.Fatalf("verify failed: %v", err)
	}

	// Tampered SHA256SUMS.txt fails --verify.
	if err := os.WriteFile(sumsPath, []byte(strings.Replace(sumsContent, "0", "1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--verify", "--pubkey", pubPath, sumsPath}, getenv, &out, &errBuf); err == nil {
		t.Fatal("verify accepted tampered SHA256SUMS.txt")
	}
}
