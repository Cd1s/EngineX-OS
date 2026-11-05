package webui

// Wi-Fi Drop (#66): the console uploads audio files from a computer straight
// onto a mounted drive, into <drive>/Wi-Fi Drop/<playlist>/. Each file is
// streamed to disk as it arrives (never held in RAM or in /tmp, which is RAM),
// names are cut down to one plain component, existing files are never
// overwritten, and 256 MiB always stays free. FLAC/MP3 dropped on the drive
// whose library stream-sync imports into are analysed and added to that
// playlist like downloads; everything else shows in Engine's file browser and
// is analysed when loaded.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	dropDir     = "Wi-Fi Drop"
	dropReserve = 256 << 20
	maxDropFile = 4<<30 - 1 // FAT32's limit
	dropCheck   = 64 << 20  // free space is checked again after this many bytes
)

var dropTypes = map[string]bool{".mp3": true, ".flac": true, ".wav": true, ".aiff": true, ".aif": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true}

type dropDrive struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // internal (eMMC), hd (internal SATA), sd, usb, or drive when unknown
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
	Path  string `json:"path"`
}

// driveKind tells the deck's drives apart by their block device: mmcblk0 is
// the deck's own eMMC (az01-internal), other mmcblk are SD slots, and a SCSI disk is USB when its sysfs path runs through a USB host.
func driveKind(dev, sysPath string) string {
	switch {
	case strings.HasPrefix(dev, "/dev/mmcblk0"):
		return "internal"
	case strings.HasPrefix(dev, "/dev/mmcblk"):
		return "sd"
	case strings.HasPrefix(dev, "/dev/sd") && strings.Contains(sysPath, "/usb"):
		return "usb"
	case strings.HasPrefix(dev, "/dev/sd") && sysPath != "":
		return "hd"
	}
	return "drive"
}

func mountDevices() map[string]string {
	out := map[string]string{}
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) >= 2 {
			out[strings.ReplaceAll(fields[1], `\040`, " ")] = fields[0]
		}
	}
	return out
}

func freeSpace(path string) (free, total int64, readOnly bool, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, false, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), st.Flags&unix_ST_RDONLY != 0, nil
}

const unix_ST_RDONLY = 0x1 // ST_RDONLY from statvfs(3)

// dropDrives lists the writable volumes under the media root.
func (s *Server) dropDrives() []dropDrive {
	root := s.mediaRoot()
	entries, _ := os.ReadDir(root)
	devs := mountDevices()
	out := []dropDrive{}
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(root, e.Name())
		free, total, ro, err := freeSpace(p)
		if err != nil || ro {
			continue
		}
		dev := devs[p]
		sys, _ := filepath.EvalSymlinks(filepath.Join("/sys/class/block", filepath.Base(dev)))
		out = append(out, dropDrive{Name: e.Name(), Kind: driveKind(dev, sys), Free: free, Total: total, Path: p})
	}
	return out
}

func (s *Server) dropDrivesGet(w http.ResponseWriter, r *http.Request) { respond(w, s.dropDrives()) }

// plainName accepts one visible path component that FAT/exFAT can store.
func plainName(n string) bool {
	if n == "" || len(n) > 200 || n[0] == '.' || strings.TrimSpace(n) != n {
		return false
	}
	for _, r := range n {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return false
		}
	}
	return true
}

// dropFileName keeps the base name a browser sent and replaces what FAT/exFAT
// cannot store, so "Artist: Track.mp3" from a Mac still uploads.
func dropFileName(sent string) string {
	name := filepath.Base(strings.ReplaceAll(sent, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	return strings.TrimLeft(strings.TrimSpace(name), ".")
}

// dropFolder creates <drive>/Wi-Fi Drop/<playlist>, refusing symlinks on the way.
func dropFolder(drive, playlist string) (string, error) {
	dir := drive
	for _, name := range []string{dropDir, playlist} {
		dir = filepath.Join(dir, name)
		st, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(dir, 0o755); err == nil {
				continue
			}
			if st, err = os.Lstat(dir); err != nil { // made meanwhile by another upload
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is not a folder", name)
		}
	}
	return dir, nil
}

type dropWriter struct {
	f                  *os.File
	drive              string
	written, nextCheck int64
}

func (d *dropWriter) Write(b []byte) (int, error) {
	if d.written+int64(len(b)) > maxDropFile {
		return 0, errDropLarge
	}
	if d.written >= d.nextCheck {
		if free, _, _, err := freeSpace(d.drive); err != nil || free-int64(len(b)) < dropReserve {
			return 0, errDropFull
		}
		d.nextCheck += dropCheck
	}
	n, err := d.f.Write(b)
	d.written += int64(n)
	return n, err
}

var (
	errDropFull  = errors.New("not enough free space on the drive (256 MiB stays free)")
	errDropLarge = errors.New("file larger than 4 GiB")
)

// saveDrop streams src into dir/name, or "name (2).ext" and so on when the
// name is taken. Nothing is left behind on failure.
func saveDrop(drive, dir, name string, src io.Reader) (string, error) {
	tmp, err := os.CreateTemp(dir, ".wifi-drop-*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	w := &dropWriter{f: tmp, drive: drive}
	_, err = io.CopyBuffer(w, src, make([]byte, 1<<20))
	if err == nil && w.written == 0 {
		err = errors.New("empty file")
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		final := name
		if i > 1 {
			final = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		// shortcut: check-then-rename can race a second upload of the same name; FAT has no link(2) to make it atomic.
		if _, err := os.Lstat(filepath.Join(dir, final)); errors.Is(err, os.ErrNotExist) {
			return final, os.Rename(tmp.Name(), filepath.Join(dir, final))
		}
	}
	return "", errors.New("too many files with this name")
}

type dropResult struct {
	Saved    []string `json:"saved"`
	Imported int      `json:"imported"`
	Error    string   `json:"error,omitempty"`
}

// drop is POST /api/drop?drive=<name>&playlist=<name> with multipart files.
func (s *Server) drop(w http.ResponseWriter, r *http.Request) {
	// stream-sync's 30 s ReadTimeout is for API calls, not a large WAV over Wi-Fi.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(2 * time.Hour))
	_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Hour))
	q := r.URL.Query()
	var d *dropDrive
	for _, v := range s.dropDrives() {
		if v.Name == q.Get("drive") {
			d = &v
		}
	}
	if d == nil {
		fail(w, 400, errors.New("unknown or read-only drive"))
		return
	}
	playlist := q.Get("playlist")
	if playlist == "" {
		playlist = dropDir
	}
	if !plainName(playlist) {
		fail(w, 400, errors.New("invalid playlist name"))
		return
	}
	if r.ContentLength > 0 && d.Free-r.ContentLength < dropReserve {
		fail(w, 507, errDropFull)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, errors.New("multipart upload required"))
		return
	}
	dir, err := dropFolder(d.Path, playlist)
	if err != nil {
		fail(w, 500, err)
		return
	}
	res := dropResult{Saved: []string{}}
	status := 200
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			status, res.Error = 400, "upload interrupted"
			break
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := dropFileName(part.FileName())
		if !plainName(name) || !dropTypes[strings.ToLower(filepath.Ext(name))] {
			part.Close()
			status, res.Error = 400, "not an audio file: "+name
			break
		}
		final, err := saveDrop(d.Path, dir, name, part)
		part.Close()
		if err != nil {
			status, res.Error = 500, name+": "+err.Error()
			if errors.Is(err, errDropFull) {
				status = 507
			} else if errors.Is(err, errDropLarge) {
				status = 413
			}
			break
		}
		res.Saved = append(res.Saved, filepath.ToSlash(filepath.Join(dropDir, playlist, final)))
		if s.jobs != nil {
			if _, ok, _ := s.jobs.SubmitFile(filepath.Join(dir, final), playlist); ok {
				res.Imported++
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(res)
}
