// Command enginex-sign signs and verifies SHA256SUMS.txt using an Ed25519
// release key (#239). When signing, the PKCS#8 PEM private key path is read
// from the ENGINEX_RELEASE_KEY environment variable and key material is never
// printed.
package main

import (
	"crypto/ed25519"
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
	var inPath, outPath, pubPath string
	var verify bool
	fs.StringVar(&inPath, "in", "", "path to SHA256SUMS.txt")
	fs.StringVar(&outPath, "out", "", "signature path (default <in>.sig)")
	fs.BoolVar(&verify, "verify", false, "verify <in> against <out> instead of signing")
	fs.StringVar(&pubPath, "pubkey", "", "optional PKIX PEM public key file for --verify")
	if err := fs.Parse(args); err != nil {
		return err
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
		fmt.Fprintf(stdout, "verified %s against %s\n", outPath, inPath)
		return nil
	}

	keyPath := strings.TrimSpace(getenv(keyEnvVar))
	if keyPath == "" {
		return fmt.Errorf("%s environment variable is not set", keyEnvVar)
	}
	priv, err := releasesig.LoadPrivateKeyFile(keyPath)
	if err != nil {
		return err
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
