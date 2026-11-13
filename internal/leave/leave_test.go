package leave

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// A deck on 5.3.0.5 (the migrated layout of #103) with settings, keys and downloads.
func newDeck(t *testing.T) (Deck, string) {
	t.Helper()
	root := t.TempDir()
	d := filepath.Join(root, "data/enginex")
	for p, c := range map[string]string{
		"version.txt": "5.3.0.5\n", "gen2-unlocked": "1\n",
		"bundle/version.txt": "5.3.0.5\n", "bundle/release-tag": "v5.3.0.5-r1\n",
		"bundle/boot-activate.sh": "# 5.3.0.5\n", "bundle/lib/libengine-ui-hook.so": "hook 5.3.0.5",
		"ui/packs/en/Deck.qml": "5.3.0.5 pack", "ui/icons/a.png": "5.3.0.5 icon",
		"ui/.pack-version": "5.3.0.5\n", "ui/.pack-id": "id-5.3.0.5\n", "ui/settings.conf": "lang=en\n",
		"ui/safe-mode": "1\n", "state/accounts.json": "{}", "ssh_host_ed25519_key": "key", "Stream/song.flac": "music",
	} {
		write(t, filepath.Join(d, p), c)
	}
	if err := os.Symlink("enginex", filepath.Join(root, "data/sc5000-custom")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "data/AppDataUser/AIR Music Technology/Engine/.cache/qml/x.qmlc"), "stale")
	return Deck{Root: root, Stdout: io.Discard, Stderr: io.Discard}, root
}

func write(t *testing.T, p, c string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}

// legacyBundle unpacks a bundle of an older release around its genuine scripts (testdata).
func legacyBundle(t *testing.T, version string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "SCLIVE4-"+version+"-EngineX-OS-Data-Bundle")
	for _, s := range []string{"install.sh", "uninstall.sh", "boot-activate.sh"} {
		b, err := os.ReadFile(filepath.Join("testdata", "v"+version, s))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(dir, s), string(b))
	}
	write(t, filepath.Join(dir, "version.txt"), version+"\n")
	write(t, filepath.Join(dir, "alsa-dir"), "system/var/overlay/lib/alsa/conf.d\n")
	write(t, filepath.Join(dir, "99-sc5000-custom.conf"), "alsa conf\n")
	write(t, filepath.Join(dir, "bin/stream-sync"), "suite "+version)
	write(t, filepath.Join(dir, "lib/libengine-ui-hook.so"), "hook "+version)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for n, c := range map[string]string{"packs/en/Deck.qml": version + " pack", "icons/a.png": version + " icon"} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	write(t, filepath.Join(dir, "ui-packs.tar"), b.String())
	var sums []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			c, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			sums = append(sums, fmt.Sprintf("%x  %s", sha256.Sum256(c), rel))
		}
		return nil
	})
	sort.Strings(sums)
	write(t, filepath.Join(dir, "SHA256SUMS"), strings.Join(sums, "\n")+"\n")
	return dir
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return string(b)
}

// snapshot is every file under /data (through the data link), for before/after comparisons.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	base := filepath.Join(root, "data")
	filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		if strings.HasPrefix(rel, journalName) || strings.HasPrefix(rel, "AppDataUser") {
			return nil
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			out[rel] = "-> " + l
		case fi.Mode().IsRegular():
			out[rel] = read(t, p)
		}
		return nil
	})
	return out
}

func diff(a, b map[string]string) string {
	var out []string
	for k, v := range a {
		if b[k] != v {
			out = append(out, fmt.Sprintf("%s: %q -> %q", k, v, b[k]))
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			out = append(out, fmt.Sprintf("%s: new %q", k, v))
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// #234 review R1: the published 5.3.0.2 install.sh refuses the /data/sc5000-custom link every
// release since 5.3.0.3 keeps. The older release now gets its layout back: the real
// /data/sc5000-custom with every file, /data/enginex the link, and its own installer accepts it.
func TestLeaveInstallsGenuine5302AndHandsBackItsLayout(t *testing.T) {
	d, root := newDeck(t)
	target := legacyBundle(t, "5.3.0.2")
	if older, err := d.Older(target); err != nil || !older {
		t.Fatalf("5.3.0.2 under 5.3.0.5: older=%v err=%v", older, err)
	}
	if err := d.Install(target); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "data/sc5000-custom")
	if st, err := os.Lstat(old); err != nil || !st.IsDir() {
		t.Fatalf("/data/sc5000-custom is not the real directory: %v", err)
	}
	if l, err := os.Readlink(filepath.Join(root, "data/enginex")); err != nil || l != "sc5000-custom" {
		t.Fatalf("/data/enginex: %q %v, want the link to sc5000-custom", l, err)
	}
	for p, want := range map[string]string{
		"version.txt": "5.3.0.2\n", "bundle/version.txt": "5.3.0.2\n", "bundle/lib/libengine-ui-hook.so": "hook 5.3.0.2",
		"ui/packs/en/Deck.qml": "5.3.0.2 pack", "ui/icons/a.png": "5.3.0.2 icon", "ui/.pack-version": "5.3.0.2\n",
		"ui/settings.conf": "lang=en\n", "state/accounts.json": "{}", "ssh_host_ed25519_key": "key", "Stream/song.flac": "music",
	} {
		if got := read(t, filepath.Join(old, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{"gen2-unlocked", "ui/safe-mode", "bundle/" + markerName, "ui/packs/" + markerName, "bundle/release-tag"} {
		if exists(filepath.Join(old, p)) {
			t.Errorf("%s is left", p)
		}
	}
	if exists(filepath.Join(root, "data", journalName)) || exists(filepath.Join(root, "data/AppDataUser/AIR Music Technology/Engine/.cache/qml")) {
		t.Error("journal or the stale QML cache is left")
	}
	// The release's own scripts run on what it got: install.sh again, then its uninstall.sh.
	for _, s := range []string{"install.sh", "bundle/uninstall.sh"} {
		dir := target
		if s != "install.sh" {
			dir = old
		}
		cmd := exec.Command("sh", filepath.Join(dir, s))
		cmd.Dir = target
		cmd.Env = append(os.Environ(), "SC5000_CUSTOM="+old, "SC5000_ALSA_OVERLAY="+filepath.Join(root, "alsa"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("genuine 5.3.0.2 %s on the handed-back layout: %v\n%s", s, err, out)
		}
	}
}

// The published 5.3.0.3 install.sh already knows /data/enginex: the layout stays.
func TestLeaveInstallsGenuine5303InTheEnginexLayout(t *testing.T) {
	d, root := newDeck(t)
	if err := d.Install(legacyBundle(t, "5.3.0.3")); err != nil {
		t.Fatal(err)
	}
	if l, err := os.Readlink(filepath.Join(root, "data/sc5000-custom")); err != nil || l != "enginex" {
		t.Fatalf("/data/sc5000-custom: %q %v", l, err)
	}
	live := filepath.Join(root, "data/enginex")
	for p, want := range map[string]string{"version.txt": "5.3.0.3\n", "bundle/version.txt": "5.3.0.3\n",
		"ui/packs/en/Deck.qml": "5.3.0.3 pack", "gen2-unlocked": "1\n", "Stream/song.flac": "music"} {
		if got := read(t, filepath.Join(live, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
}

var errCut = errors.New("power cut")

// cutAt runs Install and stops it, like a power cut, right before step.
func cutAt(t *testing.T, d Deck, target, step string) {
	t.Helper()
	defer func() { crash = func(string) {} }()
	crash = func(s string) {
		if s == step {
			panic(errCut)
		}
	}
	defer func() {
		if r := recover(); r != errCut {
			t.Fatalf("not cut at %s: %v", step, r)
		}
	}()
	d.Install(target)
}

// #234 review R2: the published installers of 5.3.0.2 and 5.3.0.3 park the bundle and move the
// new one in with two renames, and nothing can put it back after a cut between them. Now the
// older installer only fills a stage. A cut at any step leaves a whole bundle at the path ALSA
// loads (/data/sc5000-custom/bundle/lib) and a boot-activate.sh the hook can start; before the
// commit, this release's boot-activate.sh puts every part back (Recover) and the deck is as it was.
func TestLeaveCutAtEveryStepLeavesOneWholeRelease(t *testing.T) {
	for _, version := range []string{"5.3.0.2", "5.3.0.3"} {
		steps := []string{"ui/packs", "ui/icons", "files", "bundle", "committed"}
		if version == "5.3.0.2" {
			steps = append([]string{"layout", "layout-exchanged"}, steps...)
		}
		for _, step := range steps {
			t.Run(version+"/"+step, func(t *testing.T) {
				d, root := newDeck(t)
				before := snapshot(t, root)
				delete(before, "enginex/ui/safe-mode") // the older release gets one fresh try either way
				cutAt(t, d, legacyBundle(t, version), step)
				hook := read(t, filepath.Join(root, "data/sc5000-custom/bundle/lib/libengine-ui-hook.so"))
				boot := filepath.Join(root, "data/enginex/bundle/boot-activate.sh")
				if !exists(boot) {
					boot = filepath.Join(root, "data/sc5000-custom/bundle/boot-activate.sh") // the hook's fallback
				}
				if !exists(boot) {
					t.Fatalf("no boot-activate.sh the hook can start")
				}
				if step == "committed" {
					if hook != "hook "+version {
						t.Fatalf("after the commit ALSA loads %q", hook)
					}
					return // the older release runs; its next update clears the journal
				}
				if hook != "hook 5.3.0.5" {
					t.Fatalf("before the commit ALSA loads %q", hook)
				}
				// The next boot: this release's boot-activate.sh, twice (a cut during recovery).
				for i := 0; i < 2; i++ {
					if err := d.Recover(); err != nil {
						t.Fatal(err)
					}
				}
				after := snapshot(t, root)
				delete(after, "enginex/ui/safe-mode")
				if dd := diff(before, after); dd != "" {
					t.Fatalf("not put back:\n%s", dd)
				}
				if exists(filepath.Join(root, "data", journalName)) {
					t.Fatal("journal left")
				}
			})
		}
	}
}

// A committed install leaves the journal (with this release's parts) only on a cut; the next
// release's boot clears it without touching the older release's files.
func TestLeaveRecoverAfterTheCommitOnlyClears(t *testing.T) {
	d, root := newDeck(t)
	cutAt(t, d, legacyBundle(t, "5.3.0.3"), "committed")
	before := snapshot(t, root)
	if err := d.Recover(); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, root)
	delete(before, "enginex/bundle/"+markerName)
	delete(before, "enginex/ui/packs/"+markerName)
	delete(before, "enginex/ui/icons/"+markerName)
	if dd := diff(before, after); dd != "" || exists(filepath.Join(root, "data", journalName)) {
		t.Fatalf("recover after the commit changed:\n%s", dd)
	}
}

// Without an atomic exchange (an old kernel, another file system) nothing is touched.
func TestLeaveRefusesWithoutAtomicExchange(t *testing.T) {
	defer func(e func(a, b string) error) { Exchange = e }(Exchange)
	Exchange = func(a, b string) error { return syscall.EINVAL }
	d, root := newDeck(t)
	before := snapshot(t, root)
	if err := d.Install(legacyBundle(t, "5.3.0.2")); !errors.Is(err, ErrNoExchange) {
		t.Fatalf("err = %v, want ErrNoExchange", err)
	}
	if dd := diff(before, snapshot(t, root)); dd != "" || exists(filepath.Join(root, "data", journalName)) {
		t.Fatalf("changed:\n%s", dd)
	}
}

// A damaged older bundle fails in its own install.sh, in the stage: the deck keeps this release.
func TestLeaveFailedOlderInstallChangesNothing(t *testing.T) {
	d, root := newDeck(t)
	target := legacyBundle(t, "5.3.0.2")
	write(t, filepath.Join(target, "lib/libengine-ui-hook.so"), "damaged")
	before := snapshot(t, root)
	if err := d.Install(target); err == nil {
		t.Fatal("a damaged bundle installed")
	}
	if dd := diff(before, snapshot(t, root)); dd != "" || exists(filepath.Join(root, "data", journalName)) {
		t.Fatalf("changed:\n%s", dd)
	}
}

// An exchange that fails half-way through the commit is put back at once, not at the next boot.
func TestLeaveErrorDuringTheCommitPutsBack(t *testing.T) {
	defer func(e func(a, b string) error) { Exchange = e }(Exchange)
	real := Exchange
	Exchange = func(a, b string) error {
		if strings.HasSuffix(a, "/bundle") && strings.Contains(a, journalName) {
			return syscall.EIO
		}
		return real(a, b)
	}
	d, root := newDeck(t)
	before := snapshot(t, root)
	delete(before, "enginex/ui/safe-mode")
	if err := d.Install(legacyBundle(t, "5.3.0.2")); err == nil {
		t.Fatal("no error")
	}
	after := snapshot(t, root)
	delete(after, "enginex/ui/safe-mode")
	if dd := diff(before, after); dd != "" || exists(filepath.Join(root, "data", journalName)) {
		t.Fatalf("not put back:\n%s", dd)
	}
}

// The applet: a bundle that is not older is installed in place by its own install.sh, as before.
func TestLeaveAppletInstallsNewerInPlace(t *testing.T) {
	_, root := newDeck(t)
	target := filepath.Join(t.TempDir(), "b")
	write(t, filepath.Join(target, "version.txt"), "5.3.0.6\n")
	write(t, filepath.Join(target, "install.sh"), "echo in-place > \"$ENGINEX_LEAVE_ROOT/ran\"\n")
	t.Setenv("ENGINEX_LEAVE_ROOT", root)
	var out, errb bytes.Buffer
	if code := Main([]string{"install", target}, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if got := read(t, filepath.Join(root, "ran")); got != "in-place\n" {
		t.Fatalf("ran = %q", got)
	}
}

// #234 review R1, first generation: Firmware Update Mode flashes 5.3.0.2 with nothing of this
// release running, and that release's prepare refuses the /data/sc5000-custom link. The hand-run
// layout step gives it the real directory first; run twice it changes nothing more.
func TestLeaveLayoutHandsBackWithoutInstalling(t *testing.T) {
	_, root := newDeck(t)
	before := snapshot(t, root)
	var out bytes.Buffer
	t.Setenv("ENGINEX_LEAVE_ROOT", root)
	for i := 0; i < 2; i++ {
		if code := Main([]string{"layout"}, &out, &out); code != 0 {
			t.Fatalf("code %d: %s", code, out.String())
		}
	}
	if l, err := os.Readlink(filepath.Join(root, "data/enginex")); err != nil || l != "sc5000-custom" {
		t.Fatalf("/data/enginex: %q %v", l, err)
	}
	after := map[string]string{}
	for k, v := range snapshot(t, root) {
		after[strings.Replace(k, "sc5000-custom/", "enginex/", 1)] = v
	}
	delete(before, "sc5000-custom")
	delete(after, "enginex")
	if dd := diff(before, after); dd != "" {
		t.Fatalf("files changed:\n%s", dd)
	}
	// The prepare of 5.3.0.2 refuses exactly this: a link at /data/sc5000-custom.
	if st, err := os.Lstat(filepath.Join(root, "data/sc5000-custom")); err != nil || !st.IsDir() {
		t.Fatal("/data/sc5000-custom is not a real directory")
	}
}

// #234 review round 2 (R2): the older installers write the stage with cp and tar and never sync;
// a swap is atomic, but orders no file contents before it. Every staged file and directory is
// synced before the first swap of a part.
func TestLeaveStagedPayloadIsDurableBeforeTheFirstSwap(t *testing.T) {
	defer func(e func(a, b string) error, s func(string) error) { Exchange, syncPath = e, s }(Exchange, syncPath)
	synced := map[string]bool{}
	realSync, realExchange := syncPath, Exchange
	syncPath = func(p string) error { synced[p] = true; return realSync(p) }
	d, root := newDeck(t)
	stage := filepath.Join(root, "data", journalName, "stage")
	var unsynced []string
	swaps := 0
	Exchange = func(a, b string) error {
		if !strings.Contains(a, "/probe-") {
			if swaps++; swaps == 1 {
				filepath.Walk(stage, func(p string, fi os.FileInfo, err error) error {
					if err == nil && (fi.Mode().IsRegular() || fi.IsDir()) && !synced[p] {
						unsynced = append(unsynced, p)
					}
					return nil
				})
			}
		}
		return realExchange(a, b)
	}
	if err := d.Install(legacyBundle(t, "5.3.0.2")); err != nil {
		t.Fatal(err)
	}
	if swaps == 0 || len(unsynced) > 0 {
		t.Fatalf("%d swaps; not synced before the first: %q", swaps, unsynced)
	}
}
