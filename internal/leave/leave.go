// Package leave installs an older EngineX OS release over this one on a second-generation deck
// (#234) without a moment in which the deck has no whole bundle.
//
// The older release's own install.sh, whatever its age, only writes into a stage under
// /data/.enginex-leave. This release then moves the staged parts in with atomic exchanges
// (renameat2 RENAME_EXCHANGE): the UI packs and icons, the version files and last the bundle.
// A release older than 5.3.0.3 (#103) also gets its data directory back as the real
// /data/sc5000-custom, with /data/enginex as the link. Up to the bundle exchange the live
// bundle is this release's, and its boot-activate.sh runs Recover at the next boot: a power
// cut puts back whatever had been swapped. After it the older release is whole.
// A deck that cannot exchange atomically is not downgraded: nothing is changed.
package leave

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Cd1s/EngineX-OS/internal/release"
)

const (
	journalName = ".enginex-leave"    // under /data: the stage and what a rollback needs
	markerName  = ".enginex-leave-id" // in each staged part: whose it is, wherever it ends up
	// Releases before 5.3.0.3 keep their data in the real /data/sc5000-custom (#103).
	firstEnginexLayout = "5.3.0.3"
)

// The version files install.sh writes next to the bundle.
var files = []string{"version.txt", "ui/.pack-version", "ui/.pack-id", "gen2-unlocked"}

// UI parts install.sh replaces as a whole.
var uiParts = []string{"ui/packs", "ui/icons"}

// ErrNoExchange: this kernel or file system cannot swap two paths atomically.
var ErrNoExchange = errors.New("this deck cannot swap directories atomically, so the older release is not installed (nothing was changed)")

// Exchange swaps two paths atomically; tests replace it.
var Exchange = func(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}

// crash is called before each step; tests stop there the way a power cut would.
var crash = func(step string) {}

// Deck is the deck's file system. Root is "" on a deck, a stand-in for / in tests.
type Deck struct {
	Root   string
	Stdout io.Writer
	Stderr io.Writer
}

func (d Deck) data(p ...string) string {
	return filepath.Join(append([]string{d.Root, "/data"}, p...)...)
}

func realDir(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func readLine(p string) string {
	b, _ := os.ReadFile(p)
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// live is the real data directory: /data/enginex since #103, /data/sc5000-custom before.
func (d Deck) live() (string, error) {
	for _, n := range []string{"enginex", "sc5000-custom"} {
		if realDir(d.data(n)) {
			return d.data(n), nil
		}
	}
	return "", errors.New("no EngineX OS data directory in /data")
}

// Older reports whether the bundle unpacked in target is an older release than the installed one.
func (d Deck) Older(target string) (bool, error) {
	live, err := d.live()
	if err != nil {
		return false, err
	}
	return release.Older(readLine(filepath.Join(live, "version.txt")), readLine(filepath.Join(live, "bundle/release-tag")),
		readLine(filepath.Join(target, "version.txt")), readLine(filepath.Join(target, "release-tag"))), nil
}

func writeSync(p string, b []byte) error {
	tmp := p + ".enginex-leave.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(p))
}

func syncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeSync(dst, b)
}

// relink points the symlink at p to target, atomically.
func relink(p, target string) error {
	tmp := p + ".enginex-leave.lnk"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(p))
}

// syncPath makes one file's contents or one directory's entries durable; tests record it.
var syncPath = func(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// syncTree makes a whole staged tree durable before anything of it is swapped in: the older
// installers copy with cp and tar and never sync, and an atomic swap orders no data before it.
func syncTree(root string) error {
	return filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil // a symlink is an entry of its directory, synced with that
		}
		return syncPath(p)
	})
}

func hasMarker(dir, id string) bool {
	return id != "" && readLine(filepath.Join(dir, markerName)) == id
}

// probe makes sure Exchange works here before anything is touched.
func probe(dir string) error {
	a, b := filepath.Join(dir, "probe-a"), filepath.Join(dir, "probe-b")
	if err := os.WriteFile(a, []byte("a"), 0o600); err != nil {
		return err
	}
	if err := os.Mkdir(b, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(a)
	defer os.RemoveAll(b)
	if err := Exchange(a, b); err != nil {
		return ErrNoExchange
	}
	if !realDir(a) {
		return ErrNoExchange
	}
	return nil
}

// lock serializes Install and Recover across processes (the boot run of boot-activate.sh may
// meet an install EngineX-Installer started); the kernel drops it with the process.
func (d Deck) lock(wait bool) (func(), bool) {
	f, err := os.OpenFile(d.data(journalName+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}, true // no /data to lock: nothing to protect either
	}
	how := unix.LOCK_EX
	if !wait {
		how |= unix.LOCK_NB
	}
	if unix.Flock(int(f.Fd()), how) != nil {
		f.Close()
		return nil, false
	}
	return func() { f.Close() }, true
}

// Install installs the older release unpacked in target (its install.sh, version.txt and parts).
func (d Deck) Install(target string) error {
	unlock, _ := d.lock(true)
	if unlock != nil {
		defer unlock()
	}
	if err := d.recover(); err != nil { // an earlier attempt a cut interrupted goes first
		return err
	}
	live, err := d.live()
	if err != nil {
		return err
	}
	targetVersion := readLine(filepath.Join(target, "version.txt"))
	legacy := release.Newer(firstEnginexLayout, targetVersion)
	journal := d.data(journalName)
	if err := os.MkdirAll(journal, 0o700); err != nil {
		return err
	}
	fail := func(err error) error {
		os.RemoveAll(journal)
		return err
	}
	if err := probe(journal); err != nil {
		return fail(err)
	}

	// 1. The older install.sh fills the stage, exactly as it would fill /data.
	stage, alsa := filepath.Join(journal, "stage/root"), filepath.Join(journal, "stage/alsa")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return fail(err)
	}
	cmd := exec.Command("sh", filepath.Join(target, "install.sh"))
	cmd.Dir = target
	cmd.Stdout, cmd.Stderr = d.Stdout, d.Stderr
	cmd.Env = append(withoutDest(os.Environ()),
		"SC5000_CUSTOM="+stage, "ENGINEX_CUSTOM="+stage, "SC5000_ALSA_OVERLAY="+alsa, "ENGINEX_ALSA_OVERLAY="+alsa)
	if err := cmd.Run(); err != nil {
		return fail(fmt.Errorf("the older release's install.sh failed: %w", err))
	}
	if !exists(filepath.Join(stage, "bundle/boot-activate.sh")) || readLine(filepath.Join(stage, "version.txt")) != targetVersion {
		return fail(errors.New("the older release's install.sh left no whole bundle"))
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return fail(err)
	}
	ids := hex.EncodeToString(id)
	for _, p := range append([]string{"bundle"}, uiParts...) {
		if realDir(filepath.Join(stage, p)) {
			if err := writeSync(filepath.Join(stage, p, markerName), []byte(ids+"\n")); err != nil {
				return fail(err)
			}
		}
	}
	old := filepath.Join(journal, "old")
	if err := os.MkdirAll(filepath.Join(old, "ui"), 0o700); err != nil {
		return fail(err)
	}
	for _, f := range files {
		if src := filepath.Join(live, f); exists(src) {
			if err := copyFile(src, filepath.Join(old, f)); err != nil {
				return fail(err)
			}
		} else if err := writeSync(filepath.Join(old, f+".absent"), nil); err != nil {
			return fail(err)
		}
	}
	if legacy && live == d.data("enginex") {
		if err := writeSync(filepath.Join(journal, "layout"), nil); err != nil {
			return fail(err)
		}
	}
	// The staged payload, the backups and their directories reach the disk before the id, and
	// so before the first swap (#234 review round 2).
	if err := syncTree(journal); err != nil {
		return fail(err)
	}
	// From here a cut is put back by Recover: the id says the stage is complete. So is an error.
	if err := writeSync(filepath.Join(journal, "id"), []byte(ids+"\n")); err != nil {
		return fail(err)
	}
	if err := d.commit(journal, stage, live); err != nil {
		if rerr := d.recover(); rerr != nil {
			return fmt.Errorf("%w (and putting this release back: %v)", err, rerr)
		}
		return err
	}
	return nil
}

// commit moves the staged parts in; the bundle goes last.
func (d Deck) commit(journal, stage, live string) error {
	// 2. What install.sh does to /data itself, in either direction harmless.
	os.RemoveAll(d.data("AppDataUser/AIR Music Technology/Engine/.cache/qml"))
	for _, f := range []string{"safe-mode", "mods-quarantined", "boot-count", "boot-token", "boot-ready", "last-crash",
		"boot-failures", "boot-window", "boot-progress", "boot-stage", "boot-report-latest"} {
		os.Remove(filepath.Join(live, "ui", f))
	}

	// 3. Before 5.3.0.3 the data directory is the real /data/sc5000-custom (#103).
	if exists(filepath.Join(journal, "layout")) {
		crash("layout")
		link := d.data("sc5000-custom")
		if !exists(link) {
			if err := os.Symlink("enginex", link); err != nil {
				return err
			}
		}
		if err := Exchange(d.data("enginex"), link); err != nil {
			return err
		}
		crash("layout-exchanged")
		if err := relink(d.data("enginex"), "sc5000-custom"); err != nil {
			return err
		}
		live = link
	}

	// 4. The UI parts, then the version files.
	if err := os.MkdirAll(filepath.Join(live, "ui"), 0o700); err != nil {
		return err
	}
	for _, p := range uiParts {
		src, dst := filepath.Join(stage, p), filepath.Join(live, p)
		if !realDir(src) {
			continue
		}
		crash(p)
		if exists(dst) {
			if err := Exchange(src, dst); err != nil {
				return err
			}
		} else {
			if err := writeSync(filepath.Join(journal, "absent-"+filepath.Base(p)), nil); err != nil {
				return err
			}
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	crash("files")
	for _, f := range files {
		if src := filepath.Join(stage, f); exists(src) {
			if err := copyFile(src, filepath.Join(live, f)); err != nil {
				return err
			}
		} else {
			os.Remove(filepath.Join(live, f))
		}
	}

	// 5. The commit: from this exchange on the older release is the deck's.
	crash("bundle")
	if err := Exchange(filepath.Join(stage, "bundle"), filepath.Join(live, "bundle")); err != nil {
		return err
	}
	crash("committed")
	d.finish(live)
	return nil
}

// finish drops the markers and the journal (with this release's parts) after a commit.
func (d Deck) finish(live string) {
	for _, p := range append([]string{"bundle"}, uiParts...) {
		os.Remove(filepath.Join(live, p, markerName))
	}
	os.RemoveAll(d.data(journalName))
}

func withoutDest(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		switch k {
		case "SC5000_CUSTOM", "ENGINEX_CUSTOM", "SC5000_ALSA_OVERLAY", "ENGINEX_ALSA_OVERLAY":
			continue
		}
		out = append(out, e)
	}
	return out
}

// Recover puts back an install a power cut interrupted before its commit; after the commit it
// only clears what is left. boot-activate.sh runs it at every boot, before anything else. An
// install that is running right now is left alone.
func (d Deck) Recover() error {
	if !realDir(d.data(journalName)) {
		return nil
	}
	unlock, ok := d.lock(false)
	if !ok {
		return nil
	}
	defer unlock()
	return d.recover()
}

func (d Deck) recover() error {
	journal := d.data(journalName)
	if !realDir(journal) {
		return nil
	}
	id := readLine(filepath.Join(journal, "id"))
	stage := filepath.Join(journal, "stage/root")
	live, err := d.live()
	if id == "" || err != nil || !hasMarker(filepath.Join(stage, "bundle"), id) {
		// Not prepared yet (nothing was changed), or committed: the older bundle is live.
		if err == nil {
			d.finish(live)
		} else {
			os.RemoveAll(journal)
		}
		return nil
	}
	// The older bundle never went in: every other part goes back.
	old := filepath.Join(journal, "old")
	for _, f := range files {
		if src := filepath.Join(old, f); exists(src) {
			if err := copyFile(src, filepath.Join(live, f)); err != nil {
				return err
			}
		} else if exists(src + ".absent") {
			os.Remove(filepath.Join(live, f))
		}
	}
	for _, p := range uiParts {
		dst := filepath.Join(live, p)
		if !hasMarker(dst, id) {
			continue
		}
		if exists(filepath.Join(journal, "absent-"+filepath.Base(p))) {
			if err := os.Rename(dst, filepath.Join(stage, p)); err != nil {
				return err
			}
		} else if err := Exchange(dst, filepath.Join(stage, p)); err != nil {
			return err
		}
	}
	enginex, link := d.data("enginex"), d.data("sc5000-custom")
	if exists(filepath.Join(journal, "layout")) && realDir(link) {
		if t, err := os.Readlink(enginex); err == nil && t != "enginex" {
			// Fully handed back: make it the state right after the exchange, then undo that.
			if err := relink(enginex, "enginex"); err != nil {
				return err
			}
		}
		if exists(enginex) {
			if err := Exchange(link, enginex); err != nil {
				return err
			}
		} else {
			if err := os.Rename(link, enginex); err != nil {
				return err
			}
			if err := os.Symlink("enginex", link); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(journal)
}

// Handback gives the data directory the layout of releases before 5.3.0.3 (the real
// /data/sc5000-custom, /data/enginex the link to it) and installs nothing. A first-generation
// deck is flashed in Firmware Update Mode, where nothing of this release runs, and the prepare
// of 5.3.0.2 refuses the link: run by hand before such a flash (enginex-suite-exec leave layout).
// Booting this release again instead takes it back (prepare's migration, #234). Each step is
// atomic; cut half-way, /data/enginex is a link to itself, and either release takes that too.
func (d Deck) Handback() error {
	unlock, _ := d.lock(true)
	if unlock != nil {
		defer unlock()
	}
	if err := d.recover(); err != nil {
		return err
	}
	enginex, link := d.data("enginex"), d.data("sc5000-custom")
	if realDir(link) {
		return nil // already the old layout
	}
	if !realDir(enginex) {
		return errors.New("no /data/enginex to hand back")
	}
	if !exists(link) {
		if err := os.Symlink("enginex", link); err != nil {
			return err
		}
	} else if t, err := os.Readlink(link); err != nil || t != "enginex" {
		return fmt.Errorf("/data/sc5000-custom is not EngineX's link (%q), left alone", t)
	}
	if err := Exchange(enginex, link); err != nil {
		return ErrNoExchange
	}
	return relink(enginex, "sc5000-custom")
}

// Main is the enginex-suite applet "leave": install <unpacked bundle> | recover | layout.
func Main(args []string, stdout, stderr io.Writer) int {
	d := Deck{Root: os.Getenv("ENGINEX_LEAVE_ROOT"), Stdout: stdout, Stderr: stderr}
	switch {
	case len(args) == 1 && args[0] == "recover":
		if err := d.Recover(); err != nil {
			fmt.Fprintln(stderr, "leave: recover:", err)
			return 1
		}
		return 0
	case len(args) == 1 && args[0] == "layout":
		if err := d.Handback(); err != nil {
			fmt.Fprintln(stderr, "leave: layout:", err)
			return 1
		}
		fmt.Fprintln(stdout, "/data/sc5000-custom is the data directory again: a release before 5.3.0.3 can be flashed now")
		return 0
	case len(args) == 2 && args[0] == "install":
		target := args[1]
		older, err := d.Older(target)
		if err != nil {
			fmt.Fprintln(stderr, "leave:", err)
			return 1
		}
		if !older {
			// Not a downgrade: the bundle's own install.sh, in place, as before.
			cmd := exec.Command("sh", filepath.Join(target, "install.sh"))
			cmd.Dir, cmd.Stdout, cmd.Stderr = target, stdout, stderr
			if err := cmd.Run(); err != nil {
				return 1
			}
			return 0
		}
		if err := d.Install(target); err != nil {
			fmt.Fprintln(stderr, "leave:", err)
			return 1
		}
		var b bytes.Buffer
		fmt.Fprintf(&b, "EngineX OS %s installed; the deck restarts into it\n", readLine(filepath.Join(target, "version.txt")))
		stdout.Write(b.Bytes())
		return 0
	}
	fmt.Fprintln(stderr, "usage: enginex-suite leave {install <unpacked bundle>|recover|layout}")
	return 2
}
