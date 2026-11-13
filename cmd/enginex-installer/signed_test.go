package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
)

// The tests sign their mock releases with a key of their own, which is the only release key the
// installer under test trusts (#239). A test that wants other keys replaces releaseKeys.
var testReleasePub, testReleasePriv = func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub, priv
}()

func init() {
	releaseKeys = func() ([]ed25519.PublicKey, error) { return []ed25519.PublicKey{testReleasePub}, nil }
}

// signFault breaks the signature side of a mock release.
type signFault struct {
	unsigned   bool   // no SHA256SUMS.txt and no .sig among the assets
	otherKey   bool   // signed with a key the installer does not trust
	edit       bool   // a line added to the list after it was signed
	signedTag  string // the signed list is for this release tag, not the one the page shows
	noTagEntry bool   // the signed list does not name its release tag
}

func sumsOf(files map[string]string) []byte {
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", files[n], n)
	}
	return []byte(b.String())
}

// taggedSums is sumsOf plus the entry that names the release tag.
func taggedSums(files map[string]string, tag string) []byte {
	return append(sumsOf(files), []byte(releasesig.TagDigest(tag)+"  "+releasesig.TagEntryName+"\n")...)
}

// signedTLS is a TLS GitHub whose releases/latest answer of h also names the signed SHA256SUMS.txt
// and its .sig, which this server serves at /SHA256SUMS.txt and /SHA256SUMS.txt.sig. files are the
// SHA-256s (hex) the signed list gives; the list also names the tag the page showed.
func signedTLS(t *testing.T, h http.Handler, files map[string]string) *httptest.Server {
	return signedTLSWith(t, h, files, signFault{})
}

func signedTLSWith(t *testing.T, h http.Handler, files map[string]string, f signFault) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	pageTag := ""
	lists := func() (sums, sig []byte) {
		mu.Lock()
		tag := pageTag
		mu.Unlock()
		if f.signedTag != "" {
			tag = f.signedTag
		}
		sums = sumsOf(files)
		if !f.noTagEntry {
			sums = taggedSums(files, tag)
		}
		key := testReleasePriv
		if f.otherKey {
			_, key, _ = ed25519.GenerateKey(rand.Reader)
		}
		sig, err := releasesig.Sign(key, sums)
		if err != nil {
			t.Error(err)
		}
		if f.edit {
			sums = append(sums, []byte(strings.Repeat("1", 64)+"  extra-file.bin\n")...)
		}
		return sums, sig
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+sumsFile && !f.unsigned:
			sums, _ := lists()
			w.Write(sums)
		case r.URL.Path == "/"+sigFile && !f.unsigned:
			_, sig := lists()
			w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/releases/latest") && !f.unsigned:
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			var rel map[string]any
			if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &rel) == nil {
				if assets, ok := rel["assets"].([]any); ok {
					if tag, ok := rel["tag_name"].(string); ok {
						mu.Lock()
						pageTag = tag
						mu.Unlock()
					}
					base := "https://" + r.Host
					rel["assets"] = append(assets,
						map[string]any{"name": sumsFile, "browser_download_url": base + "/" + sumsFile},
						map[string]any{"name": sigFile, "browser_download_url": base + "/" + sigFile})
					json.NewEncoder(w).Encode(rel)
					return
				}
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
		default:
			h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hexSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// oneFileRelease is a release of a single file (name, content) with the given signing fault;
// files overrides what the signed list says (default: the real SHA-256 of content) and ghDigest
// what GitHub says (default: the real one).
func oneFileRelease(t *testing.T, name string, content []byte, f signFault, files map[string]string, ghDigest string) *httptest.Server {
	t.Helper()
	if files == nil {
		files = map[string]string{name: hexSHA(content)}
	}
	if ghDigest == "" {
		ghDigest = "sha256:" + hexSHA(content)
	}
	var srv *httptest.Server
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dl/"+name {
			w.Write(content)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v5.3.0.5", "assets": []map[string]string{
			{"name": name, "browser_download_url": srv.URL + "/dl/" + name, "digest": ghDigest}}})
	})
	srv = signedTLSWith(t, h, files, f)
	return srv
}

// A release the installer cannot verify is refused before anything is written, for both
// generations; the one that verifies is downloaded and its signed list is kept beside it.
func TestSignedReleaseIsRequired(t *testing.T) {
	gen1 := struct{ name, prefix string }{"SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", "SC5000"}
	gen2 := struct{ name, prefix string }{"SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img", "SCLIVE4"}
	content := []byte("firmware bytes")
	other := map[string]string{"another-file.img": hexSHA(content)}
	for _, c := range []struct {
		name    string
		fault   signFault
		files   map[string]string
		ghDig   string
		want    string // part of the error; "" = the release installs
		content []byte
	}{
		{"signed", signFault{}, nil, "", "", content},
		{"unsigned release", signFault{unsigned: true}, nil, "", "is not signed", content},
		{"signed by another key", signFault{otherKey: true}, nil, "", "is not valid", content},
		{"list edited after signing", signFault{edit: true}, nil, "", "is not valid", content},
		{"file missing from the signed list", signFault{}, other, "", "signed checksum list", content},
		{"GitHub digest disagrees with the list", signFault{}, nil, "sha256:" + strings.Repeat("ab", 32), "SHA-256", content},
		{"list of another revision (r1 under the tag of r3)", signFault{signedTag: "v5.3.0.4-r1"}, nil, "", "another release", content},
		{"list without a release tag entry", signFault{noTagEntry: true}, nil, "", "another release", content},
	} {
		for _, g := range []struct {
			gen        string
			name, pref string
			fetch      func(*options, string, string) (string, error)
		}{
			{"gen1", gen1.name, gen1.prefix, func(o *options, prefix, dir string) (string, error) { return fetchImage(ctxT(t), o, prefix) }},
			{"gen2", gen2.name, gen2.prefix, func(o *options, prefix, dir string) (string, error) { return fetchBundle(ctxT(t), o, prefix, dir) }},
		} {
			t.Run(c.name+"/"+g.gen, func(t *testing.T) {
				srv := oneFileRelease(t, g.name, c.content, c.fault, c.files, c.ghDig)
				o := testOpts(t)
				o.defaults()
				useRelease(o, srv)
				path, err := g.fetch(o, g.pref, o.WorkDir)
				if c.want == "" {
					if err != nil {
						t.Fatalf("a signed release was refused: %v", err)
					}
					if got, _ := os.ReadFile(path); string(got) != string(content) {
						t.Fatalf("downloaded %q", got)
					}
					if _, err := os.Stat(filepath.Join(o.WorkDir, sigFile)); err != nil {
						t.Errorf("the signed list was not kept beside the download: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("err = %v, want one containing %q", err, c.want)
				}
				if entries, _ := os.ReadDir(o.WorkDir); len(entries) != 0 {
					t.Errorf("a refused release left files behind: %v", entries)
				}
			})
		}
	}
}

// The signed list says one thing and the file downloaded is another: refused, nothing kept.
func TestSignedReleaseRefusesAFileThatIsNotTheSignedOne(t *testing.T) {
	const name = "SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
	signed := []byte("the signed firmware")
	srv := oneFileRelease(t, name, []byte("a replaced firmware"), signFault{}, map[string]string{name: hexSHA(signed)}, "sha256:"+hexSHA(signed))
	o := testOpts(t)
	o.defaults()
	useRelease(o, srv)
	if _, err := fetchImage(ctxT(t), o, "SC5000"); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("err = %v, want a SHA-256 mismatch", err)
	}
	if entries, _ := os.ReadDir(o.WorkDir); len(entries) != 0 {
		t.Errorf("files left behind: %v", entries)
	}
}

func TestInstallerWithoutAnEmbeddedKeyRefusesEveryRelease(t *testing.T) {
	old := releaseKeys
	releaseKeys = func() ([]ed25519.PublicKey, error) { return nil, nil }
	t.Cleanup(func() { releaseKeys = old })
	const name = "SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
	o := testOpts(t)
	o.defaults()
	useRelease(o, oneFileRelease(t, name, []byte("x"), signFault{}, nil, ""))
	if _, err := fetchImage(ctxT(t), o, "SC5000"); err == nil || !strings.Contains(err.Error(), "no trusted release key") {
		t.Fatalf("err = %v, want no trusted release key", err)
	}
}

// A file given with --image / --bundle (or kept from an earlier run) is checked against the signed
// list beside it; with no list beside it, it is the user's own build and is used with a warning.
func TestVerifyLocal(t *testing.T) {
	const name = "SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img"
	content := []byte("firmware bytes")
	setup := func(t *testing.T, files map[string]string, withSums, withSig bool) (string, *options) {
		dir := t.TempDir()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		sums := sumsOf(files)
		sig, _ := releasesig.Sign(testReleasePriv, sums)
		if withSums {
			os.WriteFile(filepath.Join(dir, sumsFile), sums, 0o644)
		}
		if withSig {
			os.WriteFile(filepath.Join(dir, sigFile), sig, 0o644)
		}
		o := testOpts(t)
		return p, o
	}
	good := map[string]string{name: hexSHA(content)}
	for _, c := range []struct {
		name           string
		files          map[string]string
		sums, sig      bool
		want, wantSaid string
	}{
		{"signed list beside the file", good, true, true, "", ""},
		{"no list beside the file: used, with a warning", good, false, false, "", "cannot be verified"},
		{"unsigned list that names the file: used, with a warning", good, true, false, "", "cannot be verified"},
		{"unsigned list that names the file with another hash: damaged copy", map[string]string{name: hexSHA([]byte("other"))}, true, false, "damaged copy", ""},
		{"unsigned list that does not name the file: used, with a warning", map[string]string{"x.img": hexSHA(content)}, true, false, "", "cannot be verified"},
		{"signature without its list: used, with a warning", good, false, true, "", "cannot be verified"},
		{"file differs from the signed list", map[string]string{name: hexSHA([]byte("other"))}, true, true, "does not match the signed SHA-256", ""},
		{"file not in the signed list", map[string]string{"x.img": hexSHA(content)}, true, true, "signed checksum list", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, o := setup(t, c.files, c.sums, c.sig)
			var out strings.Builder
			o.Out = &out
			err := verifyLocal(o, p)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if c.wantSaid != "" && !strings.Contains(out.String(), c.wantSaid) {
				t.Errorf("said %q, want it to contain %q", out.String(), c.wantSaid)
			}
		})
	}
	t.Run("signature by another key", func(t *testing.T) {
		p, o := setup(t, good, true, false)
		_, other, _ := ed25519.GenerateKey(rand.Reader)
		sig, _ := releasesig.Sign(other, sumsOf(good))
		os.WriteFile(filepath.Join(filepath.Dir(p), sigFile), sig, 0o644)
		if err := verifyLocal(o, p); err == nil || !strings.Contains(err.Error(), "is not valid") {
			t.Fatalf("err = %v", err)
		}
	})
}

// writeSidecars keeps a signed list beside a package, as the installer does after a download.
func writeSidecars(t *testing.T, dir string, files map[string]string, key ed25519.PrivateKey) {
	t.Helper()
	sums := taggedSums(files, "v5.3.0.4-r3")
	sig, err := releasesig.Sign(key, sums)
	if err != nil {
		t.Fatal(err)
	}
	if err := rememberSums(dir, sums, sig); err != nil {
		t.Fatal(err)
	}
}

// GitHub cannot be read and the installer picks up a package from an earlier run by itself: the
// user did not name it, so it must come with the signed list kept at its download (review of #239:
// an unsigned cached file used to install with only a warning).
func TestAutomaticReuseOfACachedPackageNeedsItsSignedList(t *testing.T) {
	const prefix = "PRIME4G2"
	name := prefix + "-5.3.0.4-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img"
	content := []byte("cached firmware bytes")
	good := map[string]string{name: hexSHA(content)}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string // "" = the cached package is used
	}{
		{"signed list kept at the download", func(t *testing.T, dir string) { writeSidecars(t, dir, good, testReleasePriv) }, ""},
		{"nothing kept with it", func(t *testing.T, dir string) {}, "no verified signed list"},
		{"an unsigned SHA256SUMS.txt (a build folder)", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, sumsFile), sumsOf(good), 0o644)
		}, "no verified signed list"},
		{"a list without its signature", func(t *testing.T, dir string) {
			writeSidecars(t, dir, good, testReleasePriv)
			os.Remove(filepath.Join(dir, sigFile))
		}, "no verified signed list"},
		{"a list signed by another key", func(t *testing.T, dir string) { writeSidecars(t, dir, good, other) }, "invalid signature"},
		{"a list that does not name the file", func(t *testing.T, dir string) {
			writeSidecars(t, dir, map[string]string{"another.img": hexSHA(content)}, testReleasePriv)
		}, "signed checksum list"},
		{"a list with another hash (a re-release under the same name)", func(t *testing.T, dir string) {
			writeSidecars(t, dir, map[string]string{name: hexSHA([]byte("r2 bytes"))}, testReleasePriv)
		}, "does not match the signed SHA-256"},
	} {
		t.Run(c.name, func(t *testing.T) {
			// GitHub answers 503.
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
			t.Cleanup(srv.Close)
			o := testOpts(t)
			o.defaults()
			useRelease(o, srv)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
				t.Fatal(err)
			}
			c.setup(t, dir)
			var out strings.Builder
			o.Out = &out
			path, err := fetchBundle(ctxT(t), o, prefix, dir)
			if c.want == "" {
				if err != nil || filepath.Base(path) != name {
					t.Fatalf("the verified cached package was not used: %q, %v", path, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v (path %q), want one containing %q", err, path, c.want)
			}
			if strings.Contains(out.String(), "cannot be verified") {
				t.Errorf("an unverifiable cached package went down the warn-only path: %s", out.String())
			}
		})
	}
}

// The signed list is kept next to every download, and a failure to keep it is said, not ignored.
func TestKeepSumsSaysWhenItCannotKeepTheList(t *testing.T) {
	o := testOpts(t)
	var out strings.Builder
	o.Out = &out
	keepSums(o, filepath.Join(t.TempDir(), "no-such-dir"), []byte("a"), []byte("b"))
	if !strings.Contains(out.String(), "will not be reused") {
		t.Fatalf("no warning: %q", out.String())
	}
	dir := t.TempDir()
	if err := rememberSums(dir, []byte("a"), []byte("b")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, sumsFile)); string(got) != "a" {
		t.Fatalf("sums = %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
}
