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
	msgUpdateUnsigned  = "update refused: this release is not signed"
	msgUpdateBadSig    = "update refused: the release signature is not valid"
	msgUpdateNoKey     = "update refused: this build has no trusted release key"
	msgUpdateNotListed = "update refused: the file is not in the signed checksum list"
	msgUpdateMismatch  = "update refused: the file does not match the signed checksum"
	msgUpdateOlder     = "update refused: this release is not newer than the installed one"
	msgUpdateWrongTag  = "update refused: the signature is for a different release"
	msgUpdateNoSpace   = "update refused: not enough free space on the deck"
	msgUpdateDownload  = "update download failed"
	signedFetchTimeout = 2 * time.Minute
	updateMaxDownload  = 2 << 30 // the largest *-Update.img the deck downloads
)

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

// signedAssetDigest verifies the signature of the release that downloadURL belongs to, checks
// that the signed list is the one of the release tag the deck was offered and that this release is
// newer than the installed one, and returns the signed SHA-256 (lowercase hex) of the file. On
// failure the second result is the message to show on the deck. Nothing is installed or written
// before this returns "".
//
// The tag comes from the release page, which anyone who can serve it controls, and the file names
// carry the version but not the -rN revision: so the signed list carries a tag entry
// (releasesig.TagDigest) and the tag compared with the installed release is the one it proves. A
// genuine older revision shown under a newer tag fails here, not at the file name.
func (s *Server) signedAssetDigest(downloadURL, tag string) (digest, msg string) {
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
	if err := releasesig.VerifyTag(sums, tag); err != nil {
		return "", msgUpdateWrongTag
	}
	u, err := url.Parse(downloadURL)
	if err != nil {
		return "", msgUpdateDownload
	}
	digest, err = releasesig.LookupSHA256(sums, path.Base(u.Path))
	if err != nil {
		return "", msgUpdateNotListed
	}
	if !releasesig.NewerVersion(tag, s.installedRelease()) {
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

// Free space on the deck's data volume (#239 review). Engine and the web service keep writing to
// /data (libraries, databases, caches, settings) while an update is downloaded; a download that
// fills the volume makes those writes fail with ENOSPC. So a download is started only when the
// file and, for the second generation, the unpacked bundle and the install's staged copy fit with
// updateReserve to spare, and it
// is cut off at that budget even if the server sends more than it declared.
const (
	updateReserveDefault = 256 << 20
)

var (
	updateReserve int64 = updateReserveDefault
	// freeBytes reports the free bytes of the volume of dir (false when unknown); tests replace it.
	freeBytes = func(dir string) (int64, bool) {
		free, _, _, err := freeSpace(dir)
		return free, err == nil
	}
)

// downloadBudget returns how many bytes a download into dir may take, or the refusal message.
// declared is the Content-Length (<= 0 when unknown), extra what the install needs on top of the
// download (the unpacked bundle and the install's staged copy), max the largest file the update accepts at all.
func downloadBudget(dir string, declared, extra, max int64) (limit int64, msg string) {
	free, ok := freeBytes(dir)
	if !ok {
		return max, ""
	}
	avail := free - updateReserve - extra
	if avail <= 0 || declared > avail {
		return 0, msgUpdateNoSpace
	}
	if avail < max {
		return avail, ""
	}
	return max, ""
}
