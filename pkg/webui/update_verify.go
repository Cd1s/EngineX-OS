package webui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
)

// Signed releases (#239). Every release carries SHA256SUMS.txt and SHA256SUMS.txt.sig, an
// Ed25519 signature made with the release key over the exact bytes of the sums file. A deck
// from 5.3.0.5 on installs an update only when
//
//  1. the signature verifies against a key embedded in this build,
//  2. the sums file lists the update's file, and
//  3. the file it downloaded has exactly that SHA-256.
//
// The third step is what ties the signature to the bytes that are flashed: the image is
// downloaded here and not handed to Denon's updater by URL, because that updater fetches the
// file on its own and only checks the image's own integrity fields, which a modified image
// carries too. The messages below are what the deck shows; each has a line in i18n.tsv.
const (
	msgUpdateUnsigned   = "update refused: this release is not signed"
	msgUpdateBadSig     = "update refused: the release signature is not valid"
	msgUpdateNoKey      = "update refused: this build has no trusted release key"
	msgUpdateNotListed  = "update refused: the file is not in the signed checksum list"
	msgUpdateMismatch   = "update refused: the file does not match the signed checksum"
	msgUpdateOlder      = "update refused: the file is older than the installed version"
	msgUpdateDownload   = "update download failed"
	signedFetchTimeout  = 2 * time.Minute
	updateMaxDownload   = 2 << 30 // the largest *-Update.img the deck downloads
	updateImgVersionPat = `^[A-Za-z0-9]+-([0-9][0-9.]*)-EngineX-OS-`
)

var updateImgVersionRE = regexp.MustCompile(updateImgVersionPat)

// releaseTrustedKeys returns the release keys this build trusts; tests replace it with a key
// of their own. The production list is embedded in internal/releasesig/keys.go.
var releaseTrustedKeys = releasesig.TrustedPublicKeys

// siblingURL returns the URL of the release asset called name in the same release as assetURL
// (GitHub serves a release's files from one directory).
func siblingURL(assetURL, name string) (string, error) {
	u, err := url.Parse(assetURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || path.Base(u.Path) == "." || path.Base(u.Path) == "/" {
		return "", errors.New("bad update URL")
	}
	u.Path = path.Join(path.Dir(u.Path), name)
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}

// getSmall downloads a small release file. A missing file (HTTP 404) is reported separately
// from a network failure: the first one means the release is not signed.
func (s *Server) getSmall(ctx context.Context, rawURL string, limit int64) (data []byte, missing bool, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "EngineX-OS")
	hc := s.cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: signedFetchTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, errors.New("HTTP status " + resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, false, errors.New("file too large")
	}
	return data, false, nil
}

// signedAssetDigest verifies the signature of the release that downloadURL belongs to and
// returns the signed SHA-256 (lowercase hex) of that file. On failure the second result is the
// message to show on the deck. Nothing is installed or written before this returns "".
func (s *Server) signedAssetDigest(downloadURL string) (digest, msg string) {
	sumsURL, err := siblingURL(downloadURL, releasesig.SumsAssetName)
	if err != nil {
		return "", msgUpdateDownload
	}
	sigURL, _ := siblingURL(downloadURL, releasesig.SigAssetName)
	ctx, cancel := context.WithTimeout(context.Background(), signedFetchTimeout)
	defer cancel()
	sums, missing, err := s.getSmall(ctx, sumsURL, releasesig.MaxSumsSize)
	if missing {
		return "", msgUpdateUnsigned
	}
	if err != nil {
		return "", msgUpdateDownload
	}
	sig, missing, err := s.getSmall(ctx, sigURL, releasesig.MaxSigSize)
	if missing {
		return "", msgUpdateUnsigned
	}
	if err != nil {
		return "", msgUpdateDownload
	}
	keys, err := releaseTrustedKeys()
	if err != nil || len(keys) == 0 {
		return "", msgUpdateNoKey
	}
	if err := releasesig.VerifySignature(sums, sig, keys); err != nil {
		return "", msgUpdateBadSig
	}
	u, err := url.Parse(downloadURL)
	if err != nil {
		return "", msgUpdateDownload
	}
	name := path.Base(u.Path)
	digest, err = releasesig.LookupSHA256(sums, name)
	if err != nil {
		return "", msgUpdateNotListed
	}
	// A signed sums file of an older release, replayed under a newer tag, must not roll the
	// deck back: the file name carries the version and it is covered by the signature.
	if m := updateImgVersionRE.FindStringSubmatch(name); m != nil && newerVersion(s.installedVersion(), m[1]) {
		return "", msgUpdateOlder
	}
	return digest, ""
}

// sha256OfFile returns the lowercase hex SHA-256 of the file at p.
func sha256OfFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hasDigest reports whether the file at p has the SHA-256 want.
func hasDigest(p, want string) bool {
	got, err := sha256OfFile(p)
	return err == nil && strings.EqualFold(got, want)
}
