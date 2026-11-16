// Package leave serializes bundle installation and activation. Legacy downgrade
// restoration was retired in 5.3.0.5; older releases must upgrade to the current one.
package leave

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/migration"
	"github.com/Cd1s/EngineX-OS/internal/release"
	"golang.org/x/sys/unix"
)

const journalName = ".enginex-leave"

var (
	lstat    = os.Lstat
	readFile = os.ReadFile
	openFile = os.OpenFile
	flock    = unix.Flock
)

// Deck is the deck filesystem; Root is empty outside tests.
type Deck struct {
	Root           string
	Stdout, Stderr io.Writer
}

func (d Deck) data(p ...string) string {
	return filepath.Join(append([]string{d.Root, "/data"}, p...)...)
}
func missingOK(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
func realDir(p string) (bool, error) {
	st, err := lstat(p)
	if err != nil {
		return false, missingOK(err)
	}
	return st.IsDir(), nil
}
func readLine(p string) (string, error) {
	b, err := readFile(p)
	if err != nil {
		return "", missingOK(err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line), nil
}

// live reads a pre-5.3.0.5 directory only to identify the installed release.
func (d Deck) live() (string, error) {
	for _, name := range []string{"enginex", migration.DataName} {
		if ok, err := realDir(d.data(name)); err != nil {
			return "", err
		} else if ok {
			return d.data(name), nil
		}
	}
	return "", errors.New("no EngineX OS data directory in /data")
}
func (d Deck) Older(target string) (bool, string, error) {
	live, err := d.live()
	if err != nil {
		return false, "", err
	}
	var values [4]string
	for i, p := range []string{filepath.Join(live, "version.txt"), filepath.Join(live, "bundle/release-tag"), filepath.Join(target, "version.txt"), filepath.Join(target, "release-tag")} {
		values[i], err = readLine(p)
		if err != nil {
			return false, "", err
		}
	}
	return release.Older(values[0], values[1], values[2], values[3]), values[2], nil
}
func (d Deck) lock(wait bool) (func(), error) {
	f, err := openFile(d.data(journalName+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := unix.LOCK_EX
	if !wait {
		how |= unix.LOCK_NB
	}
	if err := flock(int(f.Fd()), how); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return func() { f.Close() }, nil
}

// Install accepts repairs and upgrades using the incoming release's installer.
// No legacy destination variables or downgrade layout are written.
func (d Deck) Install(target string) error {
	unlock, err := d.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := d.recover(); err != nil {
		return err
	}
	older, version, err := d.Older(target)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^\d+(\.\d+){2,4}$`).MatchString(version) {
		return errors.New("bundle has no valid version")
	}
	if older || release.Newer("5.3.0.5", version) {
		return errors.New("downgrades to older releases are no longer supported; upgrade to the latest EngineX OS")
	}
	cmd := exec.Command("sh", filepath.Join(target, "install.sh"))
	cmd.Dir = target
	cmd.Stdout, cmd.Stderr = d.Stdout, d.Stderr
	cmd.Env = os.Environ()
	if d.Root != "" {
		cmd.Env = append(cmd.Env, "ENGINEX_CUSTOM="+d.data("enginex"), "ENGINEX_ALSA_OVERLAY="+d.data("alsa-conf.d"))
	}
	return cmd.Run()
}

// Recover recognizes a retired transaction without modifying any parked user data.
func (d Deck) Recover() error {
	unlock, err := d.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	return d.recover()
}
func (d Deck) recover() error {
	if _, err := lstat(d.data(journalName)); err == nil {
		return errors.New("a retired downgrade journal requires manual recovery; its data was left untouched")
	} else {
		return missingOK(err)
	}
}
func (d Deck) activate(args []string) error {
	unlock, err := d.lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := d.recover(); err != nil {
		return err
	}
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), "ENGINEX_LEAVE_LOCKED=1", fmt.Sprintf("ENGINEX_LEAVE_PID=%d", os.Getpid()))
	cmd.Stdout, cmd.Stderr = d.Stdout, d.Stderr
	return cmd.Run()
}

// Handback is kept as a refusal for scripts that invoke the retired operation.
func (d Deck) Handback() error {
	return errors.New("legacy layout restoration is retired; upgrade to the latest EngineX OS")
}
func Main(args []string, stdout, stderr io.Writer) int {
	d := Deck{Root: os.Getenv("ENGINEX_LEAVE_ROOT"), Stdout: stdout, Stderr: stderr}
	var err error
	switch {
	case len(args) >= 2 && args[0] == "activate":
		err = d.activate(args[1:])
	case len(args) == 1 && args[0] == "recover":
		err = d.Recover()
	case len(args) == 1 && args[0] == "layout":
		err = d.Handback()
	case len(args) == 2 && args[0] == "install":
		err = d.Install(args[1])
	default:
		fmt.Fprintln(stderr, "usage: enginex-suite leave {install <unpacked bundle>|recover|activate <script>}")
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "leave:", err)
		return 1
	}
	return 0
}
