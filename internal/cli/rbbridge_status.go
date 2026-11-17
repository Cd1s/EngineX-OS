package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultUSBPreparingPath = "/run/enginex-usb-preparing"

type usbLoadingStatus struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
	Error string `json:"error,omitempty"`
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
}

func newUSBLoadingReporter(path, kind string) *usbLoadingReporter {
	return &usbLoadingReporter{path: path, kind: kind}
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
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Ready says the library is converted and waits for the next mount.
func (r *usbLoadingReporter) Ready() {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write(usbLoadingStatus{State: "ready", Kind: r.kind})
}

// Refresh writes the last state again, so the browser keeps showing it.
func (r *usbLoadingReporter) Refresh() {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last.State != "" {
		r.write(r.last)
	}
}

func (r *usbLoadingReporter) write(st usbLoadingStatus) {
	r.last = st
	writeStatusJSON(r.path, st)
}

func (r *usbLoadingReporter) Done() {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = usbLoadingStatus{}
	_ = os.Remove(r.path)
	_ = os.Remove(r.path + ".err")
}

func (r *usbLoadingReporter) Fail(err error) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = usbLoadingStatus{}
	_ = os.Remove(r.path)
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
