package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultUSBPreparingPath = "/run/enginex-usb-preparing"

type usbLoadingStatus struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
	Error string `json:"error,omitempty"`
	Owner string `json:"owner,omitempty"` // the job that wrote it (newUSBLoadingReporter)
}

func usbLoadingStatusPath() string {
	if p := os.Getenv("ENGINEX_USB_PREPARING_PATH"); p != "" {
		return p
	}
	return defaultUSBPreparingPath
}

// detectMediaKind returns "sd" when mount is backed by /dev/mmcblk* (SD card),
// otherwise "usb".
func detectMediaKind(mount, procMountsPath string) string {
	if procMountsPath == "" {
		if env := os.Getenv("ENGINEX_PROC_MOUNTS"); env != "" {
			procMountsPath = env
		} else {
			procMountsPath = "/proc/mounts"
		}
	}
	data, err := os.ReadFile(procMountsPath)
	if err != nil {
		return "usb"
	}
	cleanMount := filepath.Clean(mount)
	bestLen := 0
	bestDev := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mp := strings.ReplaceAll(fields[1], `\040`, " ")
		mp = filepath.Clean(mp)
		if cleanMount == mp || strings.HasPrefix(cleanMount, mp+"/") {
			if len(mp) > bestLen {
				bestLen = len(mp)
				bestDev = fields[0]
			}
		}
	}
	if strings.HasPrefix(bestDev, "/dev/mmcblk") {
		return "sd"
	}
	return "usb"
}

type usbLoadingReporter struct {
	mu       sync.Mutex
	path     string
	kind     string
	state    string // "loading" when empty; "importing" for a first import after the mount (#301)
	lastAt   time.Time
	lastDone int
	last     usbLoadingStatus
	owner    string
}

// One status file serves every drive, and each drive's mount, import job and remount write it
// from processes of their own (#301). A job refreshes, ends or clears only what it wrote: its
// owner, unique per reporter, is in the file, and the file's own lock makes the
// read-then-write of another process wait.
func newUSBLoadingReporter(path, kind string) *usbLoadingReporter {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &usbLoadingReporter{path: path, kind: kind, owner: hex.EncodeToString(b[:])}
}

// locked runs f under the status files' lock (beside them, shared by every process).
func (r *usbLoadingReporter) locked(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, err := os.OpenFile(r.path+".lock", os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		defer l.Close()
		_ = syscall.Flock(int(l.Fd()), syscall.LOCK_EX)
	}
	f()
}

// owns reports whether the status at p was written by this reporter.
func (r *usbLoadingReporter) owns(p string) bool {
	b, err := os.ReadFile(p)
	var st usbLoadingStatus
	return err == nil && json.Unmarshal(b, &st) == nil && st.Owner == r.owner
}

func (r *usbLoadingReporter) Start() {
	if r == nil || r.path == "" {
		return
	}
	_ = os.Remove(r.path + ".err")
	r.Report(0, 0, true)
}

func (r *usbLoadingReporter) Report(done, total int, force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.locked(func() { r.report(done, total, force) })
}

func (r *usbLoadingReporter) report(done, total int, force bool) {
	now := time.Now()
	if !force && done > 0 && done < total && done-r.lastDone < 100 && now.Sub(r.lastAt) < 200*time.Millisecond {
		return
	}
	r.lastAt = now
	r.lastDone = done
	state := r.state
	if state == "" {
		state = "loading"
	}
	r.write(usbLoadingStatus{State: state, Kind: r.kind, Done: done, Total: total})
}

// Ready says the library is converted and opens when the stick is inserted again: the remount
// that publishes it failed (#301).
func (r *usbLoadingReporter) Ready() { r.set("ready") }

// Publishing says the library is converted and a remount is opening it (#301).
func (r *usbLoadingReporter) Publishing() { r.set("publishing") }

func (r *usbLoadingReporter) set(state string) {
	if r == nil || r.path == "" {
		return
	}
	r.locked(func() { r.write(usbLoadingStatus{State: state, Kind: r.kind}) })
}

// Refresh writes the last state again, so the browser keeps showing it; not over the status
// another stick's import or mount wrote since (one status file for every drive).
func (r *usbLoadingReporter) Refresh() {
	if r == nil || r.path == "" {
		return
	}
	r.locked(func() {
		if r.last.State != "" && r.owns(r.path) {
			r.write(r.last)
		}
	})
}

func (r *usbLoadingReporter) write(st usbLoadingStatus) {
	st.Owner = r.owner
	r.last = st
	writeStatusJSON(r.path, st)
}

func (r *usbLoadingReporter) Done() {
	if r == nil || r.path == "" {
		return
	}
	r.locked(func() {
		r.last = usbLoadingStatus{}
		for _, p := range []string{r.path, r.path + ".err"} {
			if r.owns(p) {
				_ = os.Remove(p)
			}
		}
	})
}

func (r *usbLoadingReporter) Fail(err error) {
	if r == nil || r.path == "" {
		return
	}
	r.locked(func() { r.fail(err) })
}

func (r *usbLoadingReporter) fail(err error) {
	r.last = usbLoadingStatus{}
	_ = os.Remove(r.path) // the error is the latest word, whoever wrote the status
	msg := ""
	if err != nil {
		msg = strings.Join(strings.Fields(err.Error()), " ")
		if len(msg) > 160 {
			msg = msg[:157] + "..."
		}
	}
	writeStatusJSON(r.path+".err", usbLoadingStatus{
		State: "error",
		Kind:  r.kind,
		Error: msg,
		Owner: r.owner,
	})
}

func writeStatusJSON(path string, st usbLoadingStatus) {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".enginex-usb-prep-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		return
	}
	cleanup = false
}
