package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const copyUSBCompanion = "Engine Library/enginex.profile"

type copyUSBProfile struct {
	Version int                        `json:"version"`
	Hash    string                     `json:"profileSha256"`
	UI      map[string]json.RawMessage `json:"ui"`
}

func (s *Server) copyUSBRoot(drive string) (*os.Root, error) {
	root, err := filepath.Abs(s.mediaRoot())
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, filepath.Clean(drive))
	if err != nil || !plainName(rel) {
		return nil, errors.New("select a local USB or SD drive")
	}
	i, err := os.Lstat(filepath.Join(root, rel))
	if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("drive is not available")
	}
	return os.OpenRoot(filepath.Join(root, rel))
}

func readCopyUSBFile(root *os.Root, name string) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !i.Mode().IsRegular() || i.Size() > 1<<20 {
		return nil, errors.New("profile must be a regular file under 1 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if len(b) > 1<<20 {
		return nil, errors.New("profile exceeds 1 MiB")
	}
	return b, err
}

func copyUSBHash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func (s *Server) readCopyUSB(in copyControl) (copyBundle, string, bool, error) {
	root, err := s.copyUSBRoot(in.Drive)
	if err != nil {
		return copyBundle{}, "", false, err
	}
	defer root.Close()
	b, err := readCopyUSBFile(root, "Engine Library/user.profile")
	if err != nil {
		return copyBundle{}, "", false, errors.New("could not read the native drive profile")
	}
	native, err := copyNativeExport(b)
	if err != nil {
		return copyBundle{}, "", false, err
	}
	hash := copyUSBHash(b)
	bundle := copyBundle{Version: 1, Native: native}
	var companion copyUSBProfile
	manifest, readErr := readCopyUSBFile(root, copyUSBCompanion)
	available := readErr == nil && json.Unmarshal(manifest, &companion) == nil && companion.Version == 1 && companion.Hash == hash
	if available {
		s.uiMu.Lock()
		_, _, err = copyUIMerge(s.loadUI(), companion.UI, imagePrefix(deviceCompatible()))
		s.uiMu.Unlock()
		available = err == nil
	}
	if in.IncludeUI && !available {
		return copyBundle{}, "", false, errors.New("EngineX profile is missing, invalid or belongs to a different native profile")
	}
	if available {
		bundle.UI = companion.UI
	}
	return bundle, hash, available, nil
}

func (s *Server) copyUSBPreview(in copyControl) (map[string]any, error) {
	bundle, hash, available, err := s.readCopyUSB(in)
	if err != nil {
		return nil, err
	}
	preview, err := s.copyPreview(bundle, in)
	if err != nil {
		return nil, err
	}
	preview["includeEngineXAvailable"], preview["profileHash"] = available, hash
	return preview, nil
}

func (s *Server) prepareCopyUSB(in copyControl) error {
	if !in.Confirmed {
		return errors.New("confirm the stock drive profile import first")
	}
	bundle, hash, _, err := s.readCopyUSB(in)
	if err != nil {
		return err
	}
	if !in.IncludeUI {
		bundle.UI = nil
	}
	if hash != in.ProfileHash {
		return errors.New("drive profile changed; review and confirm it again")
	}
	before, _, err := copyNativeImport(in.Native, in.Entries)
	if err != nil {
		return err
	}
	after, _, err := copyNativeImport(bundle.Native, in.Entries)
	if err != nil {
		return err
	}
	expected, err := copyNativeExport(after)
	if err != nil {
		return err
	}
	s.uiMu.Lock()
	snap, err := s.copySnapshot(before, false)
	s.uiMu.Unlock()
	if err != nil {
		return err
	}
	id := time.Now().UTC().Format(time.RFC3339Nano)
	snap.OpID, snap.Identity = id, in.Identity
	if err = s.writeCopyJSON("candidate-undo.json", snap); err != nil {
		return err
	}
	if err = restoreCopyFile(filepath.Join(s.copyDir(), "usb.profile"), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: after}); err != nil {
		return err
	}
	op := copyRecord{Identity: in.Identity, ID: id, Phase: "usb-load", Operation: "usb", Expires: time.Now().Add(2 * time.Minute), Before: before, After: after, UI: bundle.UI, Expected: expected}
	if err = s.writeCopyJSON("operation.json", op); err != nil {
		return err
	}
	path := filepath.Join(in.Drive, "Engine Library", "user.profile")
	if strings.ContainsAny(path, "\r\n") {
		return errors.New("invalid drive path")
	}
	// Only this path, only reads, and only for the next 30 seconds. The hook
	// redirects to usb.profile; neither the drive profile nor its identity fields
	// are ever written back to the user's storage.
	marker := []byte(fmt.Sprintf("%d\n%s\n", time.Now().Add(30*time.Second).Unix(), path))
	return restoreCopyFile(filepath.Join(s.copyDir(), "usb-read"), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: marker})
}

func (s *Server) exportCopyUSB(drive string) error {
	root, err := s.copyUSBRoot(drive)
	if err != nil {
		return err
	}
	defer root.Close()
	native, err := readCopyUSBFile(root, "Engine Library/user.profile")
	if err != nil {
		return err
	}
	if _, err = copyNativeExport(native); err != nil {
		return err
	}
	s.uiMu.Lock()
	ui := copyUIExport(s.loadUI())
	s.uiMu.Unlock()
	b, err := json.MarshalIndent(copyUSBProfile{Version: 1, Hash: copyUSBHash(native), UI: ui}, "", "    ")
	if err != nil {
		return err
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	name := "Engine Library/.enginex-profile-" + hex.EncodeToString(nonce[:])
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return root.Rename(name, copyUSBCompanion)
}
