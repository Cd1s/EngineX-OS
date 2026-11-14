// Package releasesig implements Ed25519 signing and verification for EngineX OS
// release checksum manifests (SHA256SUMS.txt and SHA256SUMS.txt.sig) using only
// the Go standard library (#239).
package releasesig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// SumsAssetName is the signed checksum manifest uploaded with every release.
	SumsAssetName = "SHA256SUMS.txt"
	// SigAssetName is the base64-encoded Ed25519 signature over the exact bytes of SumsAssetName.
	SigAssetName = "SHA256SUMS.txt.sig"

	// TagEntryName is the manifest entry that binds the release tag (see TagDigest).
	TagEntryName = "RELEASE-TAG.txt"

	// MaxSumsSize bounds SHA256SUMS.txt downloads (28 artifacts is ~3.5 KiB).
	MaxSumsSize = 1 << 20
	// MaxSigSize bounds SHA256SUMS.txt.sig downloads (base64 of 64 bytes is ~89 bytes).
	MaxSigSize = 4096
)

var (
	// ErrNoTrustedKeys indicates no trusted Ed25519 release public keys are configured.
	ErrNoTrustedKeys = errors.New("no trusted release public keys configured")
	// ErrInvalidSignature indicates the signature is malformed or failed Ed25519 verification.
	ErrInvalidSignature = errors.New("release signature verification failed")
	// ErrTagMismatch indicates the signed list is not the one of the release that is being installed:
	// it carries no tag entry, or the entry is for another tag (a genuine older release shown under a newer tag).
	ErrTagMismatch = errors.New("the signed list belongs to another release")
)

// TagDigest is the SHA-256 (lowercase hex) that the signed manifest lists for TagEntryName in the
// release called tag: the hash of the tag and a newline, which is exactly the content of the
// RELEASE-TAG.txt file the release build writes. A deck knows the tag it is offered, so it checks
// the entry without downloading the file; the build host's sha256sum -c checks it against the file.
// Release file names carry the version but not the "-rN" revision, so this entry is what ties a
// genuine manifest and its files to one release tag.
func TagDigest(tag string) string {
	sum := sha256.Sum256([]byte(tag + "\n"))
	return hex.EncodeToString(sum[:])
}

// VerifyTag checks that the signed list sumsData carries the tag entry of the release tag.
// The list's signature must have been verified first.
func VerifyTag(sumsData []byte, tag string) error {
	if tag == "" || len(tag) > 128 || strings.ContainsAny(tag, "\r\n\x00") {
		return ErrTagMismatch
	}
	digest, err := LookupSHA256(sumsData, TagEntryName)
	if err != nil || digest != TagDigest(tag) {
		return ErrTagMismatch
	}
	return nil
}

// ParsePublicKeysPEM parses a list of PKIX ("BEGIN PUBLIC KEY") PEM-encoded Ed25519 public keys.
func ParsePublicKeysPEM(pems []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(pems))
	for i, raw := range pems {
		pub, err := ParsePublicKeyPEM([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("trusted key %d: %w", i, err)
		}
		keys = append(keys, pub)
	}
	return keys, nil
}

// ParsePublicKeyPEM parses a single PKIX ("BEGIN PUBLIC KEY") PEM-encoded Ed25519 public key.
func ParsePublicKeyPEM(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("expected PKIX PUBLIC KEY PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("public key is not Ed25519")
	}
	return pub, nil
}

// TrustedPublicKeys parses the embedded TrustedPublicKeysPEM list.
func TrustedPublicKeys() ([]ed25519.PublicKey, error) {
	return ParsePublicKeysPEM(TrustedPublicKeysPEM)
}

// ParsePrivateKeyPEM parses a PKCS#8 ("BEGIN PRIVATE KEY") PEM-encoded Ed25519 private key.
// Error messages never include key material.
func ParsePrivateKeyPEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("expected PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKCS#8 private key")
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("private key is not Ed25519")
	}
	return priv, nil
}

// LoadPrivateKeyFile reads a root-only/owner-only (mode & 0077 == 0) PKCS#8 PEM Ed25519 private key file.
func LoadPrivateKeyFile(path string) (ed25519.PrivateKey, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("release key path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot stat release key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("release key path is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("release key file permissions too open (require owner-only, e.g. 0600)")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read release key file: %w", err)
	}
	return ParsePrivateKeyPEM(pemBytes)
}

// Sign returns the base64-encoded Ed25519 signature (with trailing newline) over the exact bytes of sumsData.
func Sign(priv ed25519.PrivateKey, sumsData []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	if len(sumsData) == 0 {
		return nil, errors.New("refusing to sign empty SHA256SUMS.txt")
	}
	sig := ed25519.Sign(priv, sumsData)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// VerifySignature verifies a base64-encoded Ed25519 signature over the exact bytes of sumsData
// against any key in keys.
func VerifySignature(sumsData, sigData []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return ErrNoTrustedKeys
	}
	if len(sumsData) == 0 {
		return errors.New("empty SHA256SUMS.txt")
	}
	trimmed := strings.TrimSpace(string(sigData))
	if trimmed == "" {
		return ErrInvalidSignature
	}
	sig, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrInvalidSignature
	}
	for _, pub := range keys {
		if len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, sumsData, sig) {
			return nil
		}
	}
	return ErrInvalidSignature
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ParseSHA256SUMS parses standard sha256sum output ("<hex>  <filename>\n") into a filename -> lowercase hex map.
func ParseSHA256SUMS(sumsData []byte) (map[string]string, error) {
	text := strings.ReplaceAll(string(sumsData), "\r\n", "\n")
	out := make(map[string]string)
	for i, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		if len(line) <= 66 || !isHex64(line[:64]) || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("malformed SHA256SUMS.txt line %d", i+1)
		}
		name := strings.TrimPrefix(line[66:], "./")
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return nil, fmt.Errorf("invalid filename on SHA256SUMS.txt line %d", i+1)
		}
		digest := strings.ToLower(line[:64])
		if prev, exists := out[name]; exists && prev != digest {
			return nil, fmt.Errorf("conflicting checksum entries for %s", name)
		}
		out[name] = digest
	}
	if len(out) == 0 {
		return nil, errors.New("SHA256SUMS.txt contains no entries")
	}
	return out, nil
}

// LookupSHA256 returns the lowercase 64-hex SHA-256 digest for assetName in sumsData.
func LookupSHA256(sumsData []byte, assetName string) (string, error) {
	if assetName == "" || strings.ContainsAny(assetName, "/\\\x00") {
		return "", errors.New("invalid asset name")
	}
	sums, err := ParseSHA256SUMS(sumsData)
	if err != nil {
		return "", err
	}
	digest, ok := sums[assetName]
	if !ok {
		return "", fmt.Errorf("%s not listed in %s", assetName, SumsAssetName)
	}
	return digest, nil
}

// VerifyAsset verifies sigData over sumsData against keys and returns the expected lowercase SHA-256 hex digest for assetName.
func VerifyAsset(sumsData, sigData []byte, assetName string, keys []ed25519.PublicKey) (string, error) {
	if err := VerifySignature(sumsData, sigData, keys); err != nil {
		return "", err
	}
	return LookupSHA256(sumsData, assetName)
}
