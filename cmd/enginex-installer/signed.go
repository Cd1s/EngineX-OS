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

	"golang.org/x/crypto/ssh"

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
	// File names carry no -rN revision: the signed tag entry ties the list to the release it is shown under.
	if err := releasesig.VerifyTag(sums, rel.Tag); err != nil {
		return nil, nil, fmt.Errorf("签名清单不属于 release %s（可能是另一个版本被换了标签），拒绝刷写 / the signed list is not the one of release %s (another release under this tag?); nothing flashed", rel.Tag, rel.Tag)
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

// rememberSums keeps the verified sums, signature and release tag in dir, atomically, so that an
// offline run can check the package it falls back to (verifyCached). The tag file is checked
// against the signed list when it is read, so it can not name another release. A failure is
// returned: without them the cached package cannot be reused later, which fails closed.
func rememberSums(dir string, sums, sig []byte, tag string) error {
	for _, f := range []struct {
		name string
		data []byte
	}{{sumsFile, sums}, {sigFile, sig}, {releasesig.TagEntryName, []byte(tag + "\n")}} {
		tmp, err := os.CreateTemp(dir, ".sums-*")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(f.data)
		cerr := tmp.Close()
		if werr == nil && cerr == nil {
			werr = os.Rename(tmp.Name(), filepath.Join(dir, f.name))
		}
		if werr != nil || cerr != nil {
			os.Remove(tmp.Name())
			if werr != nil {
				return werr
			}
			return cerr
		}
	}
	return nil
}

// keepSums calls rememberSums and says so when it cannot: the install goes on (it was verified),
// but the next offline run will not reuse this package.
func keepSums(o *options, dir string, sums, sig []byte, tag string) {
	if err := rememberSums(dir, sums, sig, tag); err != nil {
		o.say("警告：无法保存已验证的签名清单（"+err.Error()+"），下次离线时不会复用这个下载的包",
			"Warning: could not keep the verified signed list ("+err.Error()+"); this download will not be reused when GitHub cannot be reached")
	}
}

// verifyCached is for a package that was downloaded earlier and is picked up again without the
// user naming it, when GitHub cannot be read. It is not the user's own build: it must come with
// the signed list that was kept when it was downloaded, the signature must verify, and the list
// must give the file's hash. Anything less is refused; the warn-only path of verifyLocal is for
// files the user passes with --image or --bundle. It returns the release tag the list proves, for
// the same older-release check a fresh download gets.
func verifyCached(path string) (tag string, err error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	sums, serr := os.ReadFile(filepath.Join(dir, sumsFile))
	sig, gerr := os.ReadFile(filepath.Join(dir, sigFile))
	kept, terr := os.ReadFile(filepath.Join(dir, releasesig.TagEntryName))
	if serr != nil || gerr != nil || terr != nil {
		return "", fmt.Errorf("无法读取 GitHub，而本地的 %s 没有已验证的签名清单，拒绝使用 / GitHub cannot be read and the local %s has no verified signed list kept with it; nothing flashed", base, base)
	}
	keys, err := trustedKeys()
	if err != nil {
		return "", err
	}
	if err := releasesig.VerifySignature(sums, sig, keys); err != nil {
		return "", fmt.Errorf("本地 %s 的签名清单签名无效，拒绝使用 / the signed list kept with %s has an invalid signature; nothing flashed", base, base)
	}
	tag = strings.TrimSpace(string(kept))
	if err := releasesig.VerifyTag(sums, tag); err != nil {
		return "", fmt.Errorf("本地保存的发布标签与签名清单不符，拒绝使用 / the release tag kept with %s is not the one in the signed list; nothing flashed", base)
	}
	want, err := signedDigest(sums, base, "")
	if err != nil {
		return "", err
	}
	got, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf("本地 %s 与保存的签名清单里的 SHA-256 不符，拒绝使用 / %s does not match the signed SHA-256 kept with it; nothing flashed", base, base)
	}
	return tag, nil
}

// installedReleaseCmd prints what the deck says it runs, from the files its own update check reads
// (pkg/webui installedVersion and installedRelease): "V <version>" for each version file, "T <tag>"
// for each release-tag file.
const installedReleaseCmd = `for f in ` + customDirSh + `/version.txt /data/sc5000-custom/version.txt /etc/enginex/version.txt /etc/sc5000/version.txt; do ` +
	`[ -r "$f" ] && printf 'V %s\n' "$(cat "$f")"; done; ` +
	`for f in /etc/enginex/release-tag /etc/sc5000/release-tag ` + customDirSh + `/bundle/release-tag /data/sc5000-custom/bundle/release-tag; do ` +
	`[ -r "$f" ] && printf 'T %s\n' "$(cat "$f")"; done; :`

// refuseOlder stops an install of a package fetched from a release (o.release is the tag its signed
// list proves) when the deck already runs a newer one: a genuine older release replayed by whoever
// serves the release page must not take a deck back. Same release is allowed (a repair). A file the
// user names with --image or --bundle has no o.release and is theirs to choose.
func refuseOlder(ctx context.Context, o *options, cl *ssh.Client, label string) error {
	if o.release == "" {
		return nil
	}
	out, err := sshRead(ctx, cl, installedReleaseCmd)
	if err != nil {
		return fmt.Errorf("%s: cannot read the installed release: %w", label, err)
	}
	version := "dev"
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		kind, val, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch val = strings.TrimSpace(val); kind {
		case "V":
			if val != "" && (version == "dev" || releasesig.NewerVersion(val, version)) {
				version = val
			}
		case "T":
			tags = append(tags, val)
		}
	}
	if installed := releasesig.InstalledRelease(version, tags); releasesig.NewerVersion(installed, o.release) {
		return fmt.Errorf("%s 已经是 %s，比要安装的 %s 新，拒绝回退（要回退请用 --image 或 --bundle 指定旧文件）/ %s already runs %s, newer than %s; refusing to go back (to do that on purpose, name the older file with --image or --bundle)",
			label, installed, o.release, label, installed, o.release)
	}
	return nil
}

// verifyLocal checks a file the user passed with --image or --bundle. When SHA256SUMS.txt and its
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
