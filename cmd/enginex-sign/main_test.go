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

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
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
	sumsContent := strings.Repeat("0", 64) + "  SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img\n" +
		releasesig.TagDigest("v5.3.0.5-r2") + "  " + releasesig.TagEntryName + "\n"
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
	// This key is not embedded in the build: signing with it is refused unless it is asked for
	// (the release gate signs mock releases with a throwaway key). A release signed with a key
	// the decks do not trust would be refused by every one of them.
	if err := run([]string{sumsPath}, getenv, &out, &errBuf); err == nil || !strings.Contains(err.Error(), "decks would refuse") {
		t.Fatalf("signing with a key that is not embedded: %v", err)
	}
	if _, err := os.Stat(sumsPath + ".sig"); err == nil {
		t.Fatal("a signature was written for an untrusted key")
	}
	if err := run([]string{"--allow-untrusted", sumsPath}, getenv, &out, &errBuf); err != nil {
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

	// Without --pubkey the embedded keys are used, and the throwaway key is not one of them.
	if err := run([]string{"--verify", sumsPath}, getenv, &out, &errBuf); err == nil {
		t.Fatal("--verify accepted a signature of a key that is not embedded")
	}

	// The list is bound to its release tag: the right tag verifies, a relabelled one does not.
	if err := run([]string{"--verify", "--pubkey", pubPath, "--tag", "v5.3.0.5-r2", sumsPath}, getenv, &out, &errBuf); err != nil {
		t.Fatalf("verify with the list's own tag failed: %v", err)
	}
	if err := run([]string{"--verify", "--pubkey", pubPath, "--tag", "v5.3.0.5-r3", sumsPath}, getenv, &out, &errBuf); err == nil {
		t.Fatal("--verify accepted the list of r2 as the list of r3")
	}

	// Tampered SHA256SUMS.txt fails --verify.
	if err := os.WriteFile(sumsPath, []byte(strings.Replace(sumsContent, "0", "1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--verify", "--pubkey", pubPath, sumsPath}, getenv, &out, &errBuf); err == nil {
		t.Fatal("verify accepted tampered SHA256SUMS.txt")
	}
}

func TestListKeysFailsWithoutEmbeddedKey(t *testing.T) {
	var out, errBuf bytes.Buffer
	err := run([]string{"--list-keys"}, func(string) string { return "" }, &out, &errBuf)
	keys, kerr := releasesigTrusted()
	if kerr != nil {
		t.Fatal(kerr)
	}
	if (len(keys) == 0) != (err != nil) {
		t.Fatalf("--list-keys: err %v with %d embedded keys", err, len(keys))
	}
	if err == nil && strings.Count(out.String(), "\n") != len(keys) {
		t.Fatalf("--list-keys printed %q for %d keys", out.String(), len(keys))
	}
}

func releasesigTrusted() ([]ed25519.PublicKey, error) { return releasesig.TrustedPublicKeys() }

func TestSigningRefusesAListWithoutItsReleaseTag(t *testing.T) {
	dir := t.TempDir()
	_, privPath := writeTestKeys(t, dir)
	sumsPath := filepath.Join(dir, "SHA256SUMS.txt")
	if err := os.WriteFile(sumsPath, []byte(strings.Repeat("0", 64)+"  SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	getenv := func(k string) string {
		if k == keyEnvVar {
			return privPath
		}
		return ""
	}
	if err := run([]string{"--allow-untrusted", sumsPath}, getenv, &out, &errBuf); err == nil || !strings.Contains(err.Error(), releasesig.TagEntryName) {
		t.Fatalf("err = %v, want a refusal naming %s", err, releasesig.TagEntryName)
	}
	if _, err := os.Stat(sumsPath + ".sig"); err == nil {
		t.Fatal("a signature was written for a list without a tag entry")
	}
}
