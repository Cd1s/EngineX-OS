package webui

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/releasesig"
)

// The tests sign their mock releases with a key of their own, which is the only release key the
// deck under test trusts (#239). A test that wants another set of keys replaces releaseTrustedKeys.
var testReleasePub, testReleasePriv = func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub, priv
}()

func init() {
	releaseTrustedKeys = func() ([]ed25519.PublicKey, error) { return []ed25519.PublicKey{testReleasePub}, nil }
}

func trustOnly(t *testing.T, keys ...ed25519.PublicKey) {
	t.Helper()
	old := releaseTrustedKeys
	releaseTrustedKeys = func() ([]ed25519.PublicKey, error) { return keys, nil }
	t.Cleanup(func() { releaseTrustedKeys = old })
}

// releaseFault describes a mock release and the deck that is offered it; the zero value is a
// correctly signed release v5.3.0.3 for a deck on 5.3.0.2. Each field changes one thing.
type releaseFault struct {
	unsigned   bool   // no SHA256SUMS.txt and no .sig in the release
	wrongKey   bool   // signed with a key the deck does not trust
	editSums   bool   // a line added to the list after it was signed
	notListed  bool   // the signed list does not name the update file
	tamper     bool   // the file served differs from the one that was signed
	noTagEntry bool   // the signed list does not name its release tag
	tag        string // the tag the release page advertises (default v5.3.0.3)
	signedTag  string // the tag the signed list is for (default: tag)
	installed  string // the version the deck runs (default 5.3.0.2)
	installTag string // the release tag the installed bundle carries (default: none)
	// serve, when set, answers the requests for the update file itself.
	serve func(w http.ResponseWriter, r *http.Request, content []byte)
}

func (f releaseFault) apiTag() string {
	if f.tag != "" {
		return f.tag
	}
	return "v5.3.0.3"
}

func (f releaseFault) installedVersion() string {
	if f.installed != "" {
		return f.installed
	}
	return "5.3.0.2"
}

func (f releaseFault) served(content []byte) []byte {
	if !f.tamper {
		return content
	}
	b := append([]byte(nil), content...)
	b[len(b)-1] ^= 1
	return b
}

// signed returns SHA256SUMS.txt and SHA256SUMS.txt.sig for a release whose files all have the content.
func (f releaseFault) signed(names []string, content []byte) (sums, sig []byte) {
	var b strings.Builder
	if f.notListed {
		fmt.Fprintf(&b, "%064x  some-other-file.bin\n", 7)
	} else {
		for _, n := range names {
			fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(content), n)
		}
	}
	if !f.noTagEntry {
		tag := f.signedTag
		if tag == "" {
			tag = f.apiTag()
		}
		fmt.Fprintf(&b, "%s  %s\n", releasesig.TagDigest(tag), releasesig.TagEntryName)
	}
	sums = []byte(b.String())
	key := testReleasePriv
	if f.wrongKey {
		_, key, _ = ed25519.GenerateKey(rand.Reader)
	}
	sig, err := releasesig.Sign(key, sums)
	if err != nil {
		panic(err)
	}
	if f.editSums {
		sums = append(sums, []byte(fmt.Sprintf("%064x  extra-file.bin\n", 9))...)
	}
	return sums, sig
}

var assetNameRE = regexp.MustCompile(`"name":\s*"([^"]+)"`)

func assetNames(assetsJSON string) []string {
	var names []string
	for _, m := range assetNameRE.FindAllStringSubmatch(assetsJSON, -1) {
		names = append(names, m[1])
	}
	return names
}

// az01Image is the smallest file hasValidAZ01Image accepts: an AZ01 head, an EOF trailer, 50 MiB.
func az01Image() []byte {
	b := make([]byte, 50<<20+16)
	copy(b, "AZ01")
	copy(b[len(b)-16:], "EOF\x00")
	return b
}

func validGen2Img(t *testing.T, prefix, version string) []byte {
	t.Helper()
	bundle := testBundle(t, prefix, version, false)
	sum := sha256.Sum256(bundle)
	var head [gen2ImgHeader]byte
	copy(head[:], fmt.Sprintf("%s%x\n%s\n%s\n", gen2ImgMagic, sum, prefix, version))
	return append(head[:], bundle...)
}

// fakeAz01Update stands in for /usr/sbin/az01-update and records its arguments.
func fakeAz01Update(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "az01-update")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> '"+calls+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin, oldGrace := az01UpdateBin, updateRebootGrace
	az01UpdateBin, updateRebootGrace = bin, 300*time.Millisecond
	t.Cleanup(func() { az01UpdateBin, updateRebootGrace = oldBin, oldGrace })
	return calls
}

func readCalls(t *testing.T, calls string) string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// noRestart is what the deck says when az01-update returned 0 but nothing restarted it, which is
// all the stand-in does; waiting for it also waits for the grace timer, the last user of the stand-in.
const noRestart = "az01-update finished but the deck did not restart; try again"

type updateKind struct {
	name, compatible, asset string
	content                 []byte
	gen1                    bool
}

func updateKinds(t *testing.T) []updateKind {
	return []updateKind{
		{"first generation .img", "inmusic,jp07", "SC5000-5.3.0.3-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", az01Image(), true},
		{"second generation .img", "inmusic,jp21", "SCLIVE4-5.3.0.3-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img", validGen2Img(t, "SCLIVE4", "5.3.0.3"), false},
		{"second generation Data-Bundle", "inmusic,jp21", "SCLIVE4-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz", testBundle(t, "SCLIVE4", "5.3.0.3", false), false},
	}
}

func (k updateKind) assets(base string) string {
	return fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, k.asset, base+"/dl/"+k.asset)
}

// nothingInstalled fails when an update that was refused left any trace on the deck.
func nothingInstalled(t *testing.T, k updateKind, custom, calls, version string) {
	t.Helper()
	if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != version+"\n" {
		t.Errorf("%s: version.txt changed to %q", k.name, b)
	}
	if k.gen1 {
		if got := readCalls(t, calls); got != "" {
			t.Errorf("%s: az01-update ran: %q", k.name, got)
		}
		for _, f := range []string{"az01-update.img", "az01-update.img.part", "az01-update.img.url"} {
			if _, err := os.Stat(filepath.Join(custom, f)); err == nil {
				t.Errorf("%s: %s kept", k.name, f)
			}
		}
		return
	}
	if m, _ := filepath.Glob(filepath.Join(custom, "bundle", "boot-activate.sh")); len(m) != 0 {
		t.Errorf("%s: the bundle was installed", k.name)
	}
	if m, _ := filepath.Glob(filepath.Join(custom, ".ota-bundle-*")); len(m) != 0 {
		t.Errorf("%s: scratch left behind: %v", k.name, m)
	}
}

// An update whose release does not verify is refused with a fixed message, on every kind of
// update file, and nothing of it is installed or kept (#239). The release is offered all the same:
// the refusal comes when the user applies it, so the screen can say why.
func TestUpdateRefusesWhatDoesNotVerify(t *testing.T) {
	for _, c := range []struct {
		name  string
		fault releaseFault
		want  string
	}{
		{"unsigned release", releaseFault{unsigned: true}, msgUpdateUnsigned},
		{"signed with a key the deck does not trust", releaseFault{wrongKey: true}, msgUpdateBadSig},
		{"list edited after it was signed", releaseFault{editSums: true}, msgUpdateBadSig},
		{"update file not in the signed list", releaseFault{notListed: true}, msgUpdateNotListed},
		{"file differs from the signed SHA-256", releaseFault{tamper: true}, msgUpdateMismatch},
		{"list of another release than the offered tag", releaseFault{signedTag: "v5.3.0.3-r1"}, msgUpdateWrongTag},
		{"list without a release tag entry", releaseFault{noTagEntry: true}, msgUpdateWrongTag},
	} {
		for _, k := range updateKinds(t) {
			t.Run(c.name+"/"+k.name, func(t *testing.T) {
				s, custom := bundleDeckWith(t, k.compatible, k.assets, k.content, c.fault)
				calls := fakeAz01Update(t)
				if u := getUpdate(s, ""); !u.UpdateAvailable || u.DownloadURL == "" || u.Error != "" {
					t.Fatalf("the release is not offered: %+v", u)
				}
				getUpdate(s, "?apply=1")
				v := waitUpdate(t, s)
				if v.Error != c.want || v.Updating || v.Progress != 0 {
					t.Fatalf("got %+v, want the refusal %q", v, c.want)
				}
				nothingInstalled(t, k, custom, calls, "5.3.0.2")
			})
		}
	}
}

// A same-version re-release has the same file names as the release before it: only the signed tag
// entry says which revision a signed list belongs to. A genuine r1 shown under the tag of r3 must
// not take a deck that runs r2 back to r1 (the review of #239: it did), and r3 itself installs.
func TestUpdateRefusesAnEarlierRevisionShownAsALaterOne(t *testing.T) {
	for _, k := range updateKinds(t) {
		t.Run(k.name, func(t *testing.T) {
			deck := releaseFault{installed: "5.3.0.5", installTag: "v5.3.0.5-r2"}
			// Control: the genuine r3 is offered and installs.
			ok := deck
			ok.tag = "v5.3.0.5-r3"
			s, custom := bundleDeckWith(t, k.compatible, k.assets, k.content, ok)
			calls := fakeAz01Update(t)
			if u := getUpdate(s, ""); !u.UpdateAvailable || u.Latest != "v5.3.0.5-r3" {
				t.Fatalf("r3 is not offered to a deck on r2: %+v", u)
			}
			getUpdate(s, "?apply=1")
			if v := waitUpdate(t, s); k.gen1 && v.Error != noRestart || !k.gen1 && v.Error != "" {
				t.Fatalf("the genuine r3 was refused: %+v", v)
			}
			// The attack: the release page says r3, the signed list and the files are r1's.
			bad := deck
			bad.tag, bad.signedTag = "v5.3.0.5-r3", "v5.3.0.5-r1"
			s, custom = bundleDeckWith(t, k.compatible, k.assets, k.content, bad)
			calls = fakeAz01Update(t)
			getUpdate(s, "")
			getUpdate(s, "?apply=1")
			if v := waitUpdate(t, s); v.Error != msgUpdateWrongTag {
				t.Fatalf("r1 shown as r3 was not refused: %+v", v)
			}
			nothingInstalled(t, k, custom, calls, "5.3.0.5")
		})
	}
}

// A deck never goes back to an earlier release, even one the signature really covers: asked to
// install it under its own tag, it refuses because the tag is not newer than the installed one.
func TestUpdateRefusesAnOlderOrSameRelease(t *testing.T) {
	k := updateKinds(t)[2]
	for _, c := range []struct{ name, tag string }{
		{"earlier revision", "v5.3.0.5-r1"},
		{"the installed revision", "v5.3.0.5-r2"},
		{"earlier version", "v5.3.0.4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, custom := bundleDeckWith(t, k.compatible, k.assets, k.content,
				releaseFault{installed: "5.3.0.5", installTag: "v5.3.0.5-r2", tag: c.tag})
			s.updateMu.Lock()
			s.updateCache.Updating = true
			s.updateMu.Unlock()
			s.applyOnlineUpdate(s.cfg.GitHubURL+"/dl/"+k.asset, c.tag)
			s.updateMu.Lock()
			got := s.updateCache.Error
			s.updateMu.Unlock()
			if got != msgUpdateOlder {
				t.Fatalf("error %q, want %q", got, msgUpdateOlder)
			}
			nothingInstalled(t, k, custom, "", "5.3.0.5")
		})
	}
}

func TestUpdateInstallsOnlyTheSignedFile(t *testing.T) {
	for _, k := range updateKinds(t) {
		t.Run(k.name, func(t *testing.T) {
			s, custom := bundleDeck(t, k.compatible, k.assets, k.content)
			calls := fakeAz01Update(t)
			getUpdate(s, "")
			getUpdate(s, "?apply=1")
			v := waitUpdate(t, s)
			if want := map[bool]string{true: noRestart, false: ""}[k.gen1]; v.Error != want {
				t.Fatalf("a correctly signed release was not installed: %+v", v)
			}
			if k.gen1 {
				staged := filepath.Join(custom, "az01-update.img")
				if got := readCalls(t, calls); got != "--file "+staged+"\n" {
					t.Fatalf("az01-update ran as %q", got)
				}
				if !hasDigest(staged, fmt.Sprintf("%x", sha256.Sum256(k.content))) {
					t.Error("the staged image is not the signed file")
				}
			} else if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != "5.3.0.3\n" {
				t.Errorf("version.txt = %q, want 5.3.0.3", b)
			}
		})
	}
}

// A deck keeps accepting the old key while a release moves to a new one, and refuses the key
// it was never given.
func TestUpdateKeyRotation(t *testing.T) {
	k := updateKinds(t)[2]
	oldPub, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, c := range []struct {
		name  string
		keys  []ed25519.PublicKey
		want  string
		wantV string
	}{
		{"new key listed after the old one", []ed25519.PublicKey{oldPub, testReleasePub}, "", "5.3.0.3\n"},
		{"new key listed first", []ed25519.PublicKey{testReleasePub, oldPub}, "", "5.3.0.3\n"},
		{"only keys that did not sign", []ed25519.PublicKey{oldPub, otherPub}, msgUpdateBadSig, "5.3.0.2\n"},
		{"no key embedded", nil, msgUpdateNoKey, "5.3.0.2\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			trustOnly(t, c.keys...)
			s, custom := bundleDeck(t, k.compatible, k.assets, k.content)
			getUpdate(s, "")
			getUpdate(s, "?apply=1")
			v := waitUpdate(t, s)
			if v.Error != c.want {
				t.Fatalf("error %q, want %q", v.Error, c.want)
			}
			if b, _ := os.ReadFile(filepath.Join(custom, "version.txt")); string(b) != c.wantV {
				t.Errorf("version.txt = %q, want %q", b, c.wantV)
			}
		})
	}
}

// A staged image of the same URL is used again only when it is the signed file: a release
// re-uploaded under the same name must not be flashed from the earlier download.
func TestUpdateDownloadsAgainAStagedImageThatIsNotTheSignedOne(t *testing.T) {
	k := updateKinds(t)[0]
	s, custom := bundleDeck(t, k.compatible, k.assets, k.content)
	calls := fakeAz01Update(t)
	u := getUpdate(s, "")
	staged := filepath.Join(custom, "az01-update.img")
	stale := az01Image()
	stale[1000] = 0x42 // a valid-looking AZ01 file, not the signed one
	if err := os.WriteFile(staged, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged+".url", []byte(u.DownloadURL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	getUpdate(s, "?apply=1")
	if v := waitUpdate(t, s); v.Error != noRestart {
		t.Fatalf("apply: %+v", v)
	}
	if !hasDigest(staged, fmt.Sprintf("%x", sha256.Sum256(k.content))) {
		t.Fatal("the stale staged image was kept and flashed")
	}
	if got := readCalls(t, calls); got != "--file "+staged+"\n" {
		t.Fatalf("az01-update ran as %q", got)
	}
}

// The update must leave Engine room to write (review of #239): a download that does not fit with
// the reserve is refused before a byte is written, and one that turns out larger than it said it
// would be (or has no declared size) is cut off at the budget, its partial file removed.
func TestUpdateKeepsFreeSpaceForTheDeck(t *testing.T) {
	const MiB = 1 << 20
	setSpace := func(t *testing.T, free, reserve int64) {
		oldFree, oldReserve := freeBytes, updateReserve
		freeBytes = func(string) (int64, bool) { return free, true }
		updateReserve = reserve
		t.Cleanup(func() { freeBytes, updateReserve = oldFree, oldReserve })
	}
	gen1 := updateKinds(t)[0] // 50 MiB image
	bundle := updateKinds(t)[2]

	t.Run("a download that does not fit is refused before it starts", func(t *testing.T) {
		setSpace(t, 70*MiB, 30*MiB) // 40 MiB of room for a 50 MiB image
		s, custom := bundleDeck(t, gen1.compatible, gen1.assets, gen1.content)
		calls := fakeAz01Update(t)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		if v := waitUpdate(t, s); v.Error != msgUpdateNoSpace {
			t.Fatalf("got %+v, want %q", v, msgUpdateNoSpace)
		}
		nothingInstalled(t, gen1, custom, calls, "5.3.0.2")
	})
	t.Run("a bundle needs room for its unpacked files too", func(t *testing.T) {
		setSpace(t, bundleMaxUnpack+10*MiB, 30*MiB) // less than the unpacked bundle and the reserve
		s, custom := bundleDeck(t, bundle.compatible, bundle.assets, bundle.content)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		if v := waitUpdate(t, s); v.Error != msgUpdateNoSpace {
			t.Fatalf("got %+v, want %q", v, msgUpdateNoSpace)
		}
		nothingInstalled(t, bundle, custom, "", "5.3.0.2")
	})
	t.Run("a server that sends more than room is cut off at the budget", func(t *testing.T) {
		const room = 30 * MiB
		setSpace(t, room+20*MiB, 20*MiB)
		var largest int64
		var custom string
		fault := releaseFault{serve: func(w http.ResponseWriter, r *http.Request, content []byte) {
			// No Content-Length: the size is not known up front, so only the budget can stop it.
			f, _ := w.(http.Flusher)
			for off := 0; off < len(content); off += MiB {
				if st, err := os.Stat(filepath.Join(custom, "az01-update.img.part")); err == nil && st.Size() > largest {
					largest = st.Size()
				}
				end := off + MiB
				if end > len(content) {
					end = len(content)
				}
				if _, err := w.Write(content[off:end]); err != nil {
					return
				}
				if f != nil {
					f.Flush()
				}
				time.Sleep(2 * time.Millisecond)
			}
		}}
		var s *Server
		s, custom = bundleDeckWith(t, gen1.compatible, gen1.assets, gen1.content, fault)
		calls := fakeAz01Update(t)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		if v := waitUpdate(t, s); v.Error != msgUpdateNoSpace {
			t.Fatalf("got %+v, want %q", v, msgUpdateNoSpace)
		}
		if largest > room+MiB { // one chunk of slack for the moment the server looked
			t.Errorf("the partial download grew to %d MiB, the budget was %d MiB", largest/MiB, room/MiB)
		}
		nothingInstalled(t, gen1, custom, calls, "5.3.0.2")
	})
	t.Run("room enough: the update installs", func(t *testing.T) {
		setSpace(t, 400*MiB, 30*MiB)
		s, _ := bundleDeck(t, gen1.compatible, gen1.assets, gen1.content)
		fakeAz01Update(t)
		getUpdate(s, "")
		getUpdate(s, "?apply=1")
		if v := waitUpdate(t, s); v.Error != noRestart {
			t.Fatalf("got %+v", v)
		}
	})
}

func TestSiblingURL(t *testing.T) {
	for _, c := range []struct{ in, name, want string }{
		{"https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.5/SC5000-5.3.0.5-EngineX-OS-x-Update.img", "SHA256SUMS.txt", "https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.5/SHA256SUMS.txt"},
		{"https://example.com/dl/a.img?token=1#x", "SHA256SUMS.txt.sig", "https://example.com/dl/SHA256SUMS.txt.sig"},
		{"http://example.com/dl/a.img", "SHA256SUMS.txt", ""},
		{"://invalid", "SHA256SUMS.txt", ""},
		{"https://", "SHA256SUMS.txt", ""},
		{"https://example.com/", "SHA256SUMS.txt", ""},
	} {
		got, err := siblingURL(c.in, c.name)
		if (err != nil) != (c.want == "") || got != c.want {
			t.Errorf("siblingURL(%q, %q) = %q, %v; want %q", c.in, c.name, got, err, c.want)
		}
	}
}

// The signature files are release assets, not update files: a deck is never offered one.
func TestSignatureAssetsAreNeverOfferedAsTheUpdateFile(t *testing.T) {
	k := updateKinds(t)[0]
	assets := func(base string) string {
		return fmt.Sprintf(`{"name":"SHA256SUMS.txt.sig","browser_download_url":%q},{"name":"SHA256SUMS.txt","browser_download_url":%q},{"name":"RELEASE-TAG.txt","browser_download_url":%q},%s`,
			base+"/dl/SHA256SUMS.txt.sig", base+"/dl/SHA256SUMS.txt", base+"/dl/RELEASE-TAG.txt", k.assets(base))
	}
	s, _ := bundleDeck(t, k.compatible, assets, k.content)
	if u := getUpdate(s, ""); !strings.HasSuffix(u.DownloadURL, "/dl/"+k.asset) {
		t.Fatalf("offered %q", u.DownloadURL)
	}
}
