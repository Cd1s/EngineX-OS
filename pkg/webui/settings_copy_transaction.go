package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type copyUndo struct {
	OpID            string                      `json:"opID"`
	Native          map[string]copyFileSnapshot `json:"nativeFiles"`
	Identity        map[string]json.RawMessage  `json:"identity"`
	AppliedSettings string                      `json:"appliedSettings"`
	Settings        copyFileSnapshot            `json:"settings"`
	Scale           copyFileSnapshot            `json:"scale"`
	Profile         copyFileSnapshot            `json:"profile"`
	Role            string                      `json:"role"`
}

type copyRecord struct {
	Identity      map[string]json.RawMessage `json:"identity"`
	ID            string                     `json:"id"`
	Phase         string                     `json:"phase"`
	Operation     string                     `json:"operation"`
	Expires       time.Time                  `json:"expires"`
	Before        []byte                     `json:"before"`
	After         []byte                     `json:"after"`
	UI            map[string]json.RawMessage `json:"ui"`
	Expected      map[string]json.RawMessage `json:"expected"`
	PreviousDrive string                     `json:"previousDrive"`
	Error         string                     `json:"error,omitempty"`
}

func (s *Server) copyDir() string   { return filepath.Join(filepath.Dir(s.uiDir()), "settings-copy") }
func (s *Server) copyDrive() string { return filepath.Join(s.copyDir(), "EngineX-Settings") }
func (s *Server) copyProfilePath() string {
	return filepath.Join(s.copyDrive(), "Engine Library", "user.profile")
}

func (s *Server) readCopyJSON(name string, out any) error {
	snap, err := snapshotCopyFile(filepath.Join(s.copyDir(), name))
	if err != nil {
		return err
	}
	if !snap.Present {
		return os.ErrNotExist
	}
	return json.Unmarshal(snap.Bytes, out)
}

func (s *Server) writeCopyJSON(name string, value any) error {
	b, err := json.Marshal(value)
	if len(b) > 1<<20 {
		return errors.New("settings snapshot exceeds 1 MiB")
	}
	if err != nil {
		return err
	}
	return restoreCopyFile(filepath.Join(s.copyDir(), name), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: b})
}

func (s *Server) copySnapshot(profile []byte, includeNative bool) (copyUndo, error) {
	settings, err := snapshotCopyFile(filepath.Join(s.uiDir(), "settings.conf"))
	if err != nil {
		return copyUndo{}, err
	}
	scale, err := snapshotCopyFile(uiScaleEnvPath)
	if err != nil {
		return copyUndo{}, err
	}
	var native map[string]copyFileSnapshot
	if includeNative {
		native, err = s.copyNativeSnapshot()
		if err != nil {
			return copyUndo{}, err
		}
	}
	return copyUndo{AppliedSettings: copyPrivateUIHash(s.loadUI()), Native: native, Settings: settings, Scale: scale, Profile: copyFileSnapshot{Present: true, Mode: 0o600, Bytes: profile}, Role: s.loadUI().FourDeckRole}, nil
}

func (s *Server) restoreCopySnapshot(snap copyUndo) error {
	// These are fixed application-owned paths, never paths from the wire.
	if snap.Native != nil {
		for _, path := range copyNativePaths() {
			file, ok := snap.Native[path]
			if !ok || (file.Present && strings.HasSuffix(path, ".json") && !json.Valid(file.Bytes)) {
				return errors.New("native undo snapshot is incomplete or invalid")
			}
		}
	}
	return errors.Join(
		s.restoreCopyNative(snap.Native),
		restoreCopyFile(filepath.Join(s.uiDir(), "settings.conf"), snap.Settings),
		restoreCopyFile(uiScaleEnvPath, snap.Scale),
		restoreCopyFile(s.copyProfilePath(), snap.Profile),
	)
}

func (s *Server) copyRestartAllowed(in copyControl) error {
	if onDeck() && in.Identity["Player"] == nil {
		return errors.New("local Player identity is not ready")
	}
	if !in.Confirmed {
		return errors.New("confirm Engine restart and playback stop before continuing")
	}
	if in.Playing == nil || *in.Playing {
		return errors.New("pause playback before copying or undoing settings")
	}
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	if time.Since(s.shadow.ownAt) < 2*time.Second {
		for _, d := range s.shadow.own {
			if d.Playing {
				return errors.New("pause playback before copying or undoing settings")
			}
		}
	}
	return nil
}

func (s *Server) copyPreview(bundle copyBundle, in copyControl) (map[string]any, error) {
	profile, nativeSkipped, err := copyNativeImport(bundle.Native, in.Entries)
	if err != nil {
		return nil, err
	}
	s.uiMu.Lock()
	_, uiSkipped, err := copyUIMerge(s.loadUI(), bundle.UI, imagePrefix(deviceCompatible()))
	s.uiMu.Unlock()
	if err != nil {
		return nil, err
	}
	accepted, err := copyNativeExport(profile)
	if err != nil {
		return nil, err
	}
	var nativeKeys, uiKeys []string
	for key := range accepted {
		nativeKeys = append(nativeKeys, key)
	}
	for key := range bundle.UI {
		uiKeys = append(uiKeys, key)
	}
	sort.Strings(nativeKeys)
	sort.Strings(uiKeys)
	return map[string]any{"bundleHash": copyBundleHash(bundle), "native": nativeKeys, "ui": uiKeys, "skippedNative": nativeSkipped, "skippedUI": uiSkipped}, nil
}

func (s *Server) prepareCopy(bundle copyBundle, in copyControl) error {
	if err := s.copyRestartAllowed(in); err != nil {
		return err
	}
	if in.BundleHash != copyBundleHash(bundle) || time.Since(bundle.ReceivedAt) > 5*time.Minute {
		return errors.New("settings preview expired or changed; review it again")
	}
	if len(in.Native) == 0 || in.Entries == nil {
		return errors.New("native preferences are not ready")
	}
	before, _, err := copyNativeImport(in.Native, in.Entries)
	if err != nil {
		return err
	}
	after, _, err := copyNativeImport(bundle.Native, in.Entries)
	if err != nil {
		return err
	}
	s.uiMu.Lock()
	_, _, err = copyUIMerge(s.loadUI(), bundle.UI, imagePrefix(deviceCompatible()))
	s.uiMu.Unlock()
	if err != nil {
		return err
	}
	expected, err := copyNativeExport(after)
	if err != nil {
		return err
	}
	r := copyRecord{ID: time.Now().UTC().Format(time.RFC3339Nano), Phase: "restart", Operation: "copy", Expires: time.Now().Add(90 * time.Second), Before: before, After: after, UI: bundle.UI, Expected: expected, PreviousDrive: in.PreviousDrive, Identity: in.Identity}
	return s.writeCopyJSON("operation.json", r)
}

func (s *Server) prepareCopyUndo(in copyControl, failedImport bool) error {
	if err := s.copyRestartAllowed(in); err != nil {
		return err
	}
	name := "undo.json"
	if failedImport {
		var op copyRecord
		if s.readCopyJSON("operation.json", &op) != nil || op.Phase != "failed" {
			return errors.New("no failed settings copy to restore")
		}
		name = "candidate-undo.json"
	}
	var snap copyUndo
	if err := s.readCopyJSON(name, &snap); err != nil {
		return errors.New("no settings copy to undo")
	}
	if failedImport {
		var op copyRecord
		_ = s.readCopyJSON("operation.json", &op)
		if snap.OpID != op.ID {
			return errors.New("rollback snapshot does not belong to this copy")
		}
	}
	if !sameCopyIdentity(in.Identity, snap.Identity) {
		return errors.New("local Player identity changed after the copy; undo is unavailable")
	}
	s.uiMu.Lock()
	current := s.loadUI()
	s.uiMu.Unlock()
	if !failedImport && copyPrivateUIHash(current) != snap.AppliedSettings {
		return errors.New("local role or account settings changed after the copy; undo is unavailable")
	}

	if current.FourDeckRole != snap.Role {
		return errors.New("the local Player role changed after the copy; undo would replace it")
	}
	expected, err := copyNativeExport(snap.Profile.Bytes)
	if err != nil {
		return err
	}
	r := copyRecord{ID: time.Now().UTC().Format(time.RFC3339Nano), Phase: "undo-restart", Operation: "undo", Expires: time.Now().Add(90 * time.Second), Expected: expected, PreviousDrive: in.PreviousDrive, Identity: in.Identity}
	if failedImport {
		r.Operation = "rollback"
	}
	return s.writeCopyJSON("operation.json", r)
}

// Called by engine-ui-guard only after the previous Engine has exited. All
// writes run in the existing server so saveUI and ordinary UI edits share uiMu.
func (s *Server) settingsCopyPrestart(w http.ResponseWriter, r *http.Request) {
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	var op copyRecord
	if err := s.readCopyJSON("operation.json", &op); errors.Is(err, os.ErrNotExist) {
		respond(w, map[string]bool{"active": false})
		return
	} else if err != nil {
		fail(w, 500, err)
		return
	}
	if err := s.expireCopy(&op); err != nil {
		fail(w, 500, err)
		return
	}
	if op.Phase != "restart" && op.Phase != "undo-restart" && op.Phase != "restore" {
		active := op.Phase == "load"
		if !active {
			if err := s.clearCopyUSBRead(); err != nil {
				fail(w, 500, err)
				return
			}
			if err := os.Remove(filepath.Join(s.copyDir(), "active-drive")); err != nil && !errors.Is(err, os.ErrNotExist) {
				fail(w, 500, err)
				return
			}
		}
		respond(w, map[string]bool{"active": active})
		return
	}
	if copyEngineRunning() {
		fail(w, 409, errors.New("Engine must exit before restoring settings"))
		return
	}
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	var snap copyUndo
	var err error
	if op.Phase == "restart" {
		err = s.readCopyJSON("candidate-undo.json", &snap)
		if errors.Is(err, os.ErrNotExist) || (err == nil && snap.OpID != op.ID) {
			snap, err = s.copySnapshot(op.Before, true)
			snap.OpID, snap.Identity = op.ID, op.Identity
			if err == nil {
				err = s.writeCopyJSON("candidate-undo.json", snap)
			}
		}
		if err == nil {
			var next uiSettings
			next, _, err = copyUIMerge(s.loadUI(), op.UI, imagePrefix(deviceCompatible()))
			if err == nil {
				err = s.saveUI(next)
			}
		}
		if err == nil {
			err = restoreCopyFile(s.copyProfilePath(), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: op.After})
		}
	} else {
		name := "undo.json"
		if op.Operation == "rollback" {
			name = "candidate-undo.json"
		}
		err = s.readCopyJSON(name, &snap)
		if err == nil && copyPrivateUIHash(s.loadUI()) != snap.AppliedSettings {
			op.Phase, op.Error = "failed", "local role or account settings changed before Engine exited"
			err = s.writeCopyJSON("operation.json", op)
			fail(w, 409, errors.Join(errors.New(op.Error), err))
			return
		}
		if err == nil {
			op.Phase = "restore"
			err = s.writeCopyJSON("operation.json", op)
		}
		if err == nil {
			err = s.restoreCopySnapshot(snap)
		}
	}
	if err == nil {
		op.Phase = "load"
		op.Expires = time.Now().Add(5 * time.Minute)
		err = s.writeCopyJSON("operation.json", op)
		if err == nil {
			err = restoreCopyFile(filepath.Join(s.copyDir(), "active-drive"), copyFileSnapshot{Present: true, Mode: 0600, Bytes: []byte("1\n")})
		}
	}
	if err != nil {
		var rollbackErr error
		if snap.Profile.Present {
			rollbackErr = s.restoreCopySnapshot(snap)
		}
		op.Phase, op.Error = "failed", "could not apply settings"
		if rollbackErr != nil {
			op.Error += "; could not fully restore previous settings"
		} else if snap.Profile.Present && snap.OpID == op.ID {
			err = errors.Join(err, os.Remove(filepath.Join(s.copyDir(), "candidate-undo.json")))
		}
		_ = os.Remove(filepath.Join(s.copyDir(), "active-drive"))
		err = errors.Join(err, rollbackErr, s.writeCopyJSON("operation.json", op))
		fail(w, 500, fmt.Errorf("%s: %w", op.Error, err))
		return
	}
	respond(w, map[string]bool{"active": true})
}

func sameCopyNative(a, b map[string]json.RawMessage) bool {
	for key, raw := range b {
		var x, y copyNativeScalar
		if json.Unmarshal(a[key], &x) != nil || json.Unmarshal(raw, &y) != nil {
			return false
		}
		first, _ := json.Marshal(x)
		second, _ := json.Marshal(y)
		if !bytes.Equal(first, second) {
			return false
		}
	}
	return true
}

func (s *Server) finishCopy(in copyControl) error {
	var op copyRecord
	if err := s.readCopyJSON("operation.json", &op); err != nil {
		return err
	}
	if in.ID != op.ID {
		return errors.New("settings operation changed")
	}
	if op.Phase != "load" && op.Phase != "usb-load" {
		return errors.New("no native settings import is active")
	}
	if !sameCopyIdentity(in.Identity, op.Identity) {
		return errors.New("Engine changed local identity; settings import was not accepted")
	}
	if !sameCopyNative(in.Native, op.Expected) {
		return errors.New("Engine did not accept every preference; restore the previous settings")
	}
	if op.Operation == "usb" && len(op.UI) > 0 {
		s.uiMu.Lock()
		next, _, err := copyUIMerge(s.loadUI(), op.UI, imagePrefix(deviceCompatible()))
		if err == nil {
			err = s.saveUI(next)
		}
		s.uiMu.Unlock()
		if err != nil {
			return err
		}
	}
	if op.Operation == "copy" || op.Operation == "usb" {
		var undo copyUndo
		if err := s.readCopyJSON("candidate-undo.json", &undo); err != nil {
			return err
		}
		if err := s.writeCopyJSON("undo.json", undo); err != nil {
			return err
		}
	}
	for _, name := range []string{"usb-read", "active-drive", "candidate-undo.json"} {
		if err := os.Remove(filepath.Join(s.copyDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	op.Phase = "done"
	if op.Operation == "undo" {
		if err := os.Remove(filepath.Join(s.copyDir(), "undo.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.writeCopyJSON("operation.json", op)
}

// SettingsCopyBeforeEngine is invoked by the vendor launch wrapper, after the
// previous Engine has exited and before starting the next one. It does no
// service management and never signals the process that invoked it.
func SettingsCopyBeforeEngine() error {
	s := &Server{}
	if _, err := os.Stat(filepath.Join(s.copyDir(), "operation.json")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !onDeck() {
		return errHostCommand
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post("http://127.0.0.1:8080/v1/sc5000/settings-copy/prestart", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		return errors.New("settings copy preparation service is unavailable")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != 200 {
		return errors.New("settings copy preparation failed")
	}
	return nil
}

func (s *Server) settingsCopyStatus(w http.ResponseWriter, r *http.Request) {
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	var incoming, outgoing *copyPeerRequest
	if s.copyIncoming != nil && time.Now().Before(s.copyIncoming.Expires) && !s.copyIncoming.Denied && s.copyIncoming.Bundle == nil {
		incoming = s.copyIncoming
	}
	if s.copyOutgoing != nil && time.Now().Before(s.copyOutgoing.Expires) {
		outgoing = s.copyOutgoing
	}
	types := map[string]int{"TrackEndWarningTime": 0}
	for _, key := range copyNativeBools {
		types[key] = 1
	}
	for _, key := range copyNativeEnums {
		types[key] = 4
	}
	var operation copyRecord
	_ = s.readCopyJSON("operation.json", &operation)
	if err := s.expireCopy(&operation); err != nil {
		fail(w, 500, err)
		return
	}
	var undo copyUndo
	hasUndo := s.readCopyJSON("undo.json", &undo) == nil
	rollback := operation.Phase == "failed" && s.readCopyJSON("candidate-undo.json", &undo) == nil && undo.OpID == operation.ID
	if s.copyReceived != nil && time.Since(s.copyReceived.ReceivedAt) > 5*time.Minute {
		s.copyReceived = nil
	}
	opStatus := map[string]any{"id": operation.ID, "phase": operation.Phase, "operation": operation.Operation, "previousDrive": operation.PreviousDrive, "error": operation.Error}
	respond(w, map[string]any{"peers": s.copyPeers(), "incoming": incoming, "outgoing": outgoing, "received": s.copyReceived != nil,
		"nativeTypes": types, "operation": opStatus, "drive": s.copyDrive(), "undo": hasUndo, "rollback": rollback})
}

func (s *Server) settingsCopyControl(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			fail(w, 403, errors.New("same-origin request required"))
			return
		}
	}
	var in copyControl
	if err := decodeCopyRequest(w, r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	var err error
	switch in.Action {
	case "begin":
		err = s.copyBegin(in.Peer)
	case "allow", "deny":
		err = s.copyAllow(r, in)
	case "poll":
		err = s.copyPoll()
	case "cancel":
		s.copyMu.Lock()
		s.copyOutgoing, s.copyReceived = nil, nil
		var operation copyRecord
		if s.readCopyJSON("operation.json", &operation) == nil && (operation.Phase == "restart" || operation.Phase == "undo-restart") {
			err = os.Remove(filepath.Join(s.copyDir(), "operation.json"))
		}
		s.copyMu.Unlock()
	case "preview", "apply", "undo", "rollback", "finish", "failed", "usb-preview", "usb-prepare", "usb-export":
		if !copyLocalScreen(r) {
			fail(w, 403, errors.New("confirm settings changes on the receiving deck"))
			return
		}
		s.copyMu.Lock()
		defer s.copyMu.Unlock()
		var pending copyRecord
		if s.readCopyJSON("operation.json", &pending) == nil {
			if err := s.expireCopy(&pending); err != nil {
				fail(w, 500, err)
				return
			}
		}
		if in.Action == "apply" || in.Action == "usb-prepare" || in.Action == "undo" || in.Action == "rollback" {
			var current copyRecord
			if s.readCopyJSON("operation.json", &current) == nil && current.Phase != "done" && current.Phase != "failed" && current.Phase != "" {
				fail(w, 409, errors.New("another settings import is in progress"))
				return
			}
		}
		switch in.Action {
		case "preview", "apply":
			if s.copyReceived == nil {
				err = errors.New("no approved settings received")
				break
			}
			if in.Action == "preview" {
				var preview map[string]any
				preview, err = s.copyPreview(*s.copyReceived, in)
				if err == nil {
					respond(w, preview)
					return
				}
			} else {
				err = s.prepareCopy(*s.copyReceived, in)
				if err == nil {
					s.copyReceived = nil
				}
			}
		case "undo", "rollback":
			err = s.prepareCopyUndo(in, in.Action == "rollback")
		case "finish":
			err = s.finishCopy(in)
		case "failed":
			var operation copyRecord
			err = s.readCopyJSON("operation.json", &operation)
			if err == nil && (in.ID != operation.ID || (operation.Phase != "load" && operation.Phase != "usb-load" && operation.Phase != "failed")) {
				err = errors.New("no matching native import is active")
			}
			if err == nil {
				operation.Phase, operation.Error = "failed", "settings import failed; undo restores the previous settings"
				if in.Loading != nil && !*in.Loading {
					err = s.clearCopyUSBRead()
				}
				if err == nil {
					err = s.writeCopyJSON("operation.json", operation)
				}
			}
		case "usb-preview":
			var preview map[string]any
			preview, err = s.copyUSBPreview(in)
			if err == nil {
				respond(w, preview)
				return
			}
		case "usb-prepare":
			err = s.prepareCopyUSB(in)
		case "usb-export":
			err = s.exportCopyUSB(in.Drive)
		}
	default:
		err = errors.New("unknown settings copy action")
	}
	if err != nil {
		fail(w, 409, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}

func copyLocalScreen(r *http.Request) bool {
	ip := net.ParseIP(copyRemoteIP(r))
	return ip != nil && ip.IsLoopback() && !relayedFromOutside(r) && strings.HasPrefix(r.URL.Path, "/v1/sc5000/")
}

func copyBundleHash(bundle copyBundle) string {
	b, _ := json.Marshal(bundle)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func copyPrivateUIHash(settings uiSettings) string {
	b, _ := json.Marshal(settings)
	var values map[string]json.RawMessage
	_ = json.Unmarshal(b, &values)
	for key := range values {
		if copyUIAllowed(key) {
			delete(values, key)
		}
	}
	b, _ = json.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func sameCopyIdentity(a, b map[string]json.RawMessage) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// File names are derived locally, never supplied by a peer or a USB profile.
// Native persistence remains owned by Engine; raw restoration runs only after
// it exits, before its next start. Snapshots never leave this deck.
var copyNativeDataRoot = "/data"

func copyNativePaths() []string {
	base := "AppDataUser/AIR Music Technology/Engine/State/"
	paths := []string{base + "PlanckClientState.json", base + "PlanckEngineState.json", base + "PlanckGuiState.json"}
	for _, compatible := range deviceCompatible() {
		if imagePrefixes[compatible] != "" {
			board := strings.TrimPrefix(compatible, "inmusic,")
			paths = append(paths, ".config/"+board+".user.settings/AIR Music Technology/Engine.conf")
			break
		}
	}
	return paths
}
func (s *Server) copyNativeSnapshot() (map[string]copyFileSnapshot, error) {
	result := make(map[string]copyFileSnapshot)
	for _, path := range copyNativePaths() {
		snap, err := snapshotCopyFile(filepath.Join(copyNativeDataRoot, path))
		if err != nil {
			return nil, err
		}
		if snap.Present && strings.HasSuffix(path, ".json") && !json.Valid(snap.Bytes) {
			return nil, errors.New("native settings file is invalid")
		}
		result[path] = snap
	}
	return result, nil
}
func (s *Server) restoreCopyNative(files map[string]copyFileSnapshot) error {
	// USB imports cannot flush native identity without restarting Engine.
	// Their undo restores the private profile through the stock loader instead.
	if files == nil {
		return nil
	}
	for _, path := range copyNativePaths() {
		if _, ok := files[path]; !ok {
			return errors.New("native undo snapshot is incomplete")
		}
	}
	for _, path := range copyNativePaths() {
		snap, ok := files[path]
		if !ok {
			return errors.New("native undo snapshot is incomplete")
		}
		if err := restoreCopyFile(filepath.Join(copyNativeDataRoot, path), snap); err != nil {
			return err
		}
	}
	return nil
}
func copyEngineRunning() bool {
	if !onDeck() {
		return false
	}
	paths, _ := filepath.Glob("/proc/[0-9]*/comm")
	for _, path := range paths {
		b, _ := os.ReadFile(path)
		if strings.TrimSpace(string(b)) == "Engine" {
			return true
		}
	}
	return false
}
func (s *Server) expireCopy(op *copyRecord) error {
	if op.Phase == "restore" || op.Phase == "" || op.Phase == "done" || op.Phase == "failed" || time.Now().Before(op.Expires) {
		return nil
	}
	op.Phase, op.Error = "failed", "settings import expired; restore the previous settings"
	for _, name := range []string{"active-drive"} {
		if err := os.Remove(filepath.Join(s.copyDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return s.writeCopyJSON("operation.json", op)
}

// Keep filtering a timed-out native load until it actually completes or exits.
// Clearing the marker earlier could expose a delayed read to the full USB profile.
func (s *Server) clearCopyUSBRead() error {
	err := os.Remove(filepath.Join(s.copyDir(), "usb-read"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
