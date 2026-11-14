// Command enginex-sign signs and verifies SHA256SUMS.txt using an Ed25519
// release key (#239). When signing, the PKCS#8 PEM private key path is read
// from the ENGINEX_RELEASE_KEY environment variable and key material is never
// printed.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
)

const keyEnvVar = "ENGINEX_RELEASE_KEY"

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("enginex-sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var inPath, outPath, pubPath, tag string
	var verify, listKeys, allowUntrusted bool
	fs.StringVar(&inPath, "in", "", "path to SHA256SUMS.txt")
	fs.StringVar(&outPath, "out", "", "signature path (default <in>.sig)")
	fs.BoolVar(&verify, "verify", false, "verify <in> against <out> instead of signing")
	fs.StringVar(&pubPath, "pubkey", "", "optional PKIX PEM public key file for --verify")
	fs.StringVar(&tag, "tag", "", "with --verify: also check that the list is the one of this release tag (e.g. v5.3.0.5-r2)")
	fs.BoolVar(&listKeys, "list-keys", false, "print the trusted public keys embedded in this build and exit (fails when there are none)")
	fs.BoolVar(&allowUntrusted, "allow-untrusted", false, "sign with a key that is not embedded in this build (release-gate throwaway keys only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if listKeys {
		return printTrustedKeys(stdout)
	}
	if inPath == "" {
		if fs.NArg() == 1 {
			inPath = fs.Arg(0)
		} else if fs.NArg() == 0 {
			return errors.New("missing input file (pass SHA256SUMS.txt or --in)")
		} else {
			return errors.New("expected a single input file")
		}
	} else if fs.NArg() > 0 {
		return errors.New("unexpected positional arguments with --in")
	}
	if outPath == "" {
		outPath = inPath + ".sig"
	}

	sumsData, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", inPath, err)
	}
	if _, err := releasesig.ParseSHA256SUMS(sumsData); err != nil {
		return fmt.Errorf("invalid %s: %w", filepath.Base(inPath), err)
	}

	if verify {
		sigData, err := os.ReadFile(outPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", outPath, err)
		}
		var keys []ed25519.PublicKey
		if pubPath != "" {
			pubBytes, err := os.ReadFile(pubPath)
			if err != nil {
				return fmt.Errorf("read %s: %w", pubPath, err)
			}
			pub, err := releasesig.ParsePublicKeyPEM(pubBytes)
			if err != nil {
				return fmt.Errorf("parse %s: %w", pubPath, err)
			}
			keys = []ed25519.PublicKey{pub}
		} else {
			keys, err = releasesig.TrustedPublicKeys()
			if err != nil {
				return err
			}
		}
		if err := releasesig.VerifySignature(sumsData, sigData, keys); err != nil {
			return err
		}
		if tag != "" {
			if err := releasesig.VerifyTag(sumsData, tag); err != nil {
				return fmt.Errorf("%s is not the list of release %s: %w", filepath.Base(inPath), tag, err)
			}
		}
		fmt.Fprintf(stdout, "verified %s against %s\n", outPath, inPath)
		return nil
	}

	// Release file names carry no -rN revision: the tag entry is what ties this list to one release.
	if _, err := releasesig.LookupSHA256(sumsData, releasesig.TagEntryName); err != nil {
		return fmt.Errorf("%s has no %s entry: a list that does not name its release tag would verify for any tag", filepath.Base(inPath), releasesig.TagEntryName)
	}
	keyPath := strings.TrimSpace(getenv(keyEnvVar))
	if keyPath == "" {
		return fmt.Errorf("%s environment variable is not set", keyEnvVar)
	}
	priv, err := releasesig.LoadPrivateKeyFile(keyPath)
	if err != nil {
		return err
	}
	// A release signed with a key the decks do not trust would be refused by every one of them.
	if !allowUntrusted {
		if err := requireTrusted(priv); err != nil {
			return err
		}
	}
	sigData, err := releasesig.Sign(priv, sumsData)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(outPath, sigData, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s -> %s\n", inPath, outPath)
	return nil
}

// requireTrusted fails unless the public half of priv is one of the keys embedded in this build.
func requireTrusted(priv ed25519.PrivateKey) error {
	keys, err := releasesig.TrustedPublicKeys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("no trusted release public key is embedded (internal/releasesig/keys.go): decks would refuse this release")
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	for _, k := range keys {
		if k.Equal(pub) {
			return nil
		}
	}
	return errors.New("the release key is not among the embedded trusted public keys (internal/releasesig/keys.go): decks would refuse this release")
}

// printTrustedKeys lists the embedded keys as "<index> <fingerprint> <base64 SPKI>": the
// fingerprint is the first 16 hex digits of the SHA-256 of the raw key, the last field is the
// single base64 line of the PEM body, which is how the key appears inside the built binaries.
func printTrustedKeys(w io.Writer) error {
	keys, err := releasesig.TrustedPublicKeys()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("no trusted release public key is embedded (internal/releasesig/keys.go)")
	}
	for i, k := range keys {
		der, err := x509.MarshalPKIXPublicKey(k)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(k)
		fmt.Fprintf(w, "%d %s %s\n", i, hex.EncodeToString(sum[:8]), base64.StdEncoding.EncodeToString(der))
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".enginex-sign-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "enginex-sign:", err)
		}
		os.Exit(1)
	}
}
