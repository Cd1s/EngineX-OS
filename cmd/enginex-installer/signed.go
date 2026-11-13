package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
)

// Signed releases (#239). Every release has SHA256SUMS.txt and SHA256SUMS.txt.sig, an Ed25519
// signature over the exact bytes of the sums file made with the release key. The installer
// flashes only files whose SHA-256 is listed in a sums file whose signature verifies against a
// key embedded here. GitHub's own per-asset "digest" is no longer the root of trust: it only
// has to agree with the signed list.

// releaseKeys returns the trusted release public keys; tests replace it.
var releaseKeys = releasesig.TrustedPublicKeys

// Where the verified sums of the last download are kept, next to the downloaded files, so an
// offline run can check the package it falls back to.
const (
	sumsFile = releasesig.SumsAssetName
	sigFile  = releasesig.SigAssetName
)

func trustedKeys() ([]ed25519.PublicKey, error) {
	keys, err := releaseKeys()
	if err != nil || len(keys) == 0 {
		return nil, errors.New("这个安装器没有内置可信发布密钥，拒绝刷写 / this installer has no trusted release key; nothing flashed")
	}
	return keys, nil
}

func getBytes(ctx context.Context, o *options, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "enginex-installer")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

// signedSums downloads SHA256SUMS.txt and its signature from rel, verifies the signature and
// returns the sums. A release without them is refused: nothing it lists can be trusted.
func signedSums(ctx context.Context, o *options, rel *release) (sums, sig []byte, err error) {
	var sumsURL, sigURL string
	for _, a := range rel.Assets {
		if !strings.HasPrefix(a.URL, "https://") {
			continue
		}
		switch a.Name {
		case sumsFile:
			sumsURL = a.URL
		case sigFile:
			sigURL = a.URL
		}
	}
	if sumsURL == "" || sigURL == "" {
		return nil, nil, fmt.Errorf("release %s 没有签名（缺少 %s），拒绝刷写 / release %s is not signed (no %s); nothing flashed", rel.Tag, sigFile, rel.Tag, sigFile)
	}
	keys, err := trustedKeys()
	if err != nil {
		return nil, nil, err
	}
	if sums, err = getBytes(ctx, o, sumsURL, releasesig.MaxSumsSize); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", sumsFile, err)
	}
	if sig, err = getBytes(ctx, o, sigURL, releasesig.MaxSigSize); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", sigFile, err)
	}
	if err := releasesig.VerifySignature(sums, sig, keys); err != nil {
		return nil, nil, fmt.Errorf("release %s 的签名无效，拒绝刷写 / the signature of release %s is not valid; nothing flashed", rel.Tag, rel.Tag)
	}
	return sums, sig, nil
}

// signedDigest is the SHA-256 the verified sums give for the release file name. When GitHub also
// names a digest it must be the same, or the release is inconsistent and refused.
func signedDigest(sums []byte, name, githubDigest string) (string, error) {
	digest, err := releasesig.LookupSHA256(sums, name)
	if err != nil {
		return "", fmt.Errorf("%s 不在已签名的校验清单里，拒绝刷写 / %s is not in the signed checksum list; nothing flashed", name, name)
	}
	if g := strings.TrimPrefix(githubDigest, "sha256:"); strings.HasPrefix(githubDigest, "sha256:") && len(g) == 64 && !strings.EqualFold(g, digest) {
		return "", fmt.Errorf("%s 的 GitHub SHA-256 与签名清单不一致，拒绝刷写 / the GitHub SHA-256 of %s differs from the signed list; nothing flashed", name, name)
	}
	return digest, nil
}

// rememberSums keeps the verified sums and signature in dir (best effort) for offline runs.
func rememberSums(dir string, sums, sig []byte) {
	_ = os.WriteFile(filepath.Join(dir, sumsFile), sums, 0o644)
	_ = os.WriteFile(filepath.Join(dir, sigFile), sig, 0o644)
}

// verifyLocal checks a package that was not downloaded in this run (--bundle, --image, or the
// copy kept from an earlier run when GitHub cannot be read). When SHA256SUMS.txt and its
// signature sit next to it, as in a folder of release downloads, the file must be in the signed
// list with its hash; a bad signature or a different hash is refused. A file without a signed
// list beside it is the user's own build (a build folder has an unsigned SHA256SUMS.txt too): it
// is used, with a warning, unless an unsigned list names it with another hash, which is a damaged copy.
func verifyLocal(o *options, path string) error {
	dir := filepath.Dir(path)
	sums, serr := os.ReadFile(filepath.Join(dir, sumsFile))
	sig, gerr := os.ReadFile(filepath.Join(dir, sigFile))
	base := filepath.Base(path)
	if serr != nil || gerr != nil {
		if serr == nil {
			if want, err := releasesig.LookupSHA256(sums, base); err == nil {
				if got, err := fileSHA256(path); err != nil || !strings.EqualFold(got, want) {
					return fmt.Errorf("%s 与旁边 %s 里的 SHA-256 不符，文件已损坏，拒绝刷写 / %s does not match the SHA-256 in the %s beside it: damaged copy; nothing flashed", base, sumsFile, base, sumsFile)
				}
			}
		}
		o.say("警告：旁边没有签名的 "+sumsFile+"，无法验证 "+base+"，按你的指定继续",
			"Warning: no signed "+sumsFile+" next to "+base+", so it cannot be verified; using it as you chose")
		return nil
	}
	keys, err := trustedKeys()
	if err != nil {
		return err
	}
	if err := releasesig.VerifySignature(sums, sig, keys); err != nil {
		return fmt.Errorf("%s 旁边的 %s 签名无效，拒绝刷写 / the signature of the %s next to %s is not valid; nothing flashed", base, sumsFile, sumsFile, base)
	}
	want, err := signedDigest(sums, base, "")
	if err != nil {
		return err
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s 与签名清单里的 SHA-256 不符，拒绝刷写 / %s does not match the signed SHA-256; nothing flashed", base, base)
	}
	return nil
}
