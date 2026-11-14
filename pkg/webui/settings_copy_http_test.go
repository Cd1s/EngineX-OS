package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func copyTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ui"), 0700); err != nil {
		t.Fatal(err)
	}
	oldNative := copyNativeDataRoot
	copyNativeDataRoot = filepath.Join(root, "native")
	t.Cleanup(func() { copyNativeDataRoot = oldNative })
	s := New(Config{UIDir: filepath.Join(root, "ui"), StateDir: filepath.Join(root, "state"), MediaRoot: filepath.Join(root, "media"), AccessToken: "TEST-CODE"}, nil)
	t.Cleanup(s.Close)
	return s, root
}

func copyTestRequest(s *Server, method, path, body, remote, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

// This complete test can be copied to main unchanged: existing public APIs
// compile there, and the missing protected settings-copy route returns 404.
func TestSettingsCopyRouteRequiresAuthentication(t *testing.T) {
	s, _ := copyTestServer(t)
	for _, tc := range []struct {
		name, auth string
		code       int
	}{
		{"no code", "", 401}, {"wrong code", "Bearer WRONG", 401}, {"authenticated", "Bearer TEST-CODE", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := copyTestRequest(s, http.MethodGet, "/api/settings-copy", "", "192.0.2.10:1234", tc.auth)
			if w.Code != tc.code {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSettingsCopySourceConsentPeerBindingAndOneUse(t *testing.T) {
	s, _ := copyTestServer(t)
	s.shadow.mu.Lock()
	s.shadow.peers["192.0.2.20"] = time.Now()
	s.shadow.mu.Unlock()
	token := "11111111111111111111111111111111"
	w := copyTestRequest(s, "POST", "/peer/settings-copy/request", `{"token":"`+token+`"}`, "192.0.2.20:1234", "")
	if w.Code != 200 {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	w = copyTestRequest(s, "GET", "/api/settings-copy/transfer", "", "192.0.2.20:1234", "SettingsCopy "+token)
	if w.Code != 202 || strings.Contains(w.Body.String(), "native") {
		t.Fatalf("settings escaped before Allow: %d %s", w.Code, w.Body.String())
	}
	body := `{"action":"allow","token":"` + token + `","native":{"DisplayFileName":{"type":1,"state":false}}}`
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", body, "127.0.0.1:1234", "Bearer TEST-CODE")
	if w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body.String())
	}
	w = copyTestRequest(s, "GET", "/api/settings-copy/transfer", "", "192.0.2.21:1234", "SettingsCopy "+token)
	if w.Code != 401 {
		t.Fatal("grant was not bound to the requester")
	}
	w = copyTestRequest(s, "GET", "/api/settings-copy/transfer", "", "192.0.2.20:1234", "SettingsCopy "+token)
	if w.Code != 200 {
		t.Fatalf("transfer: %d %s", w.Code, w.Body.String())
	}
	for _, forbidden := range []string{"accessCode", "TEST-CODE", "scope", "fourDeckRole", "PlayerColor", "relayUrl", "githubRepo", "verboseLog"} {
		if strings.Contains(w.Body.String(), `"`+forbidden+`"`) {
			t.Errorf("payload leaks %s", forbidden)
		}
	}
	w = copyTestRequest(s, "GET", "/api/settings-copy/transfer", "", "192.0.2.20:1234", "SettingsCopy "+token)
	if w.Code != 401 {
		t.Fatal("transfer grant replayed")
	}
}

func TestSettingsCopyRestartConsentAndByteExactUndo(t *testing.T) {
	s, root := copyTestServer(t)
	oldScale := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(root, "ui-scale.env")
	t.Cleanup(func() { uiScaleEnvPath = oldScale })
	original := []byte("# byte-exact local formatting\r\nlang=en\r\nwaveform=denon\nfour_deck_role=24\nprivate_local_future_key=keep\n\n")
	path := filepath.Join(s.uiDir(), "settings.conf")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	s.copyReceived = &copyBundle{Version: 1, ReceivedAt: time.Now(), Native: map[string]json.RawMessage{"DisplayFileName": json.RawMessage(`{"type":1,"state":true}`)}, UI: map[string]json.RawMessage{"waveform": json.RawMessage(`"rgb"`), "uiScale": json.RawMessage(`85`)}}
	base := `"bundleHash":"` + copyBundleHash(*s.copyReceived) + `","native":{"DisplayFileName":{"type":1,"state":false}},"entries":{"DisplayFileName":[]}`
	for _, body := range []string{`{"action":"apply","confirmed":false,"playing":false,` + base + `}`, `{"action":"apply","confirmed":true,"playing":true,` + base + `}`} {
		w := copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", body, "127.0.0.1:1234", "")
		if w.Code == 200 {
			t.Fatal("copy bypassed confirmation/playback refusal")
		}
	}
	w := copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", `{"action":"apply","confirmed":true,"playing":false,`+base+`}`, "127.0.0.1:1234", "")
	if w.Code != 200 {
		t.Fatalf("prepare: %d %s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("confirmation wrote settings before Engine exit")
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy/prestart", `{}`, "127.0.0.1:1234", "")
	if w.Code != 200 {
		t.Fatalf("prestart: %d %s", w.Code, w.Body.String())
	}
	s.uiMu.Lock()
	changed := s.loadUI()
	s.uiMu.Unlock()
	if changed.Waveform != "rgb" || changed.FourDeckRole != "24" || changed.UIScale != 85 {
		t.Fatalf("wrong applied settings: %+v", changed)
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", `{"action":"finish","id":"`+copyTestOpID(t, s)+`","native":{"DisplayFileName":{"type":1,"state":true}}}`, "127.0.0.1:1234", "")
	if w.Code != 200 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	var undo copyUndo
	if err := s.readCopyJSON("undo.json", &undo); err != nil {
		t.Fatal(err)
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", `{"action":"undo","confirmed":true,"playing":false}`, "127.0.0.1:1234", "")
	if w.Code != 200 {
		t.Fatalf("undo consent: %d %s", w.Code, w.Body.String())
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy/prestart", `{}`, "127.0.0.1:1234", "")
	if w.Code != 200 {
		t.Fatalf("undo prestart: %d %s", w.Code, w.Body.String())
	}
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatalf("undo changed original bytes: %q", got)
	}
	profile, err := os.ReadFile(s.copyProfilePath())
	if err != nil || !bytes.Equal(profile, undo.Profile.Bytes) {
		t.Fatalf("native snapshot bytes: %v", err)
	}
	if _, err := os.Stat(uiScaleEnvPath); !os.IsNotExist(err) {
		t.Fatal("undo did not restore scale file absence")
	}
	i, _ := os.Stat(path)
	if i.Mode().Perm() != 0600 {
		t.Fatalf("undo changed mode: %v", i.Mode())
	}
}

func TestSettingsCopyUSBCompanionBoundToNativeProfileAndNoRestart(t *testing.T) {
	s, root := copyTestServer(t)
	oldScale := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(root, "ui-scale.env")
	t.Cleanup(func() { uiScaleEnvPath = oldScale })
	restarts := 0
	s.cfg.RestartEngine = func() error { restarts++; return nil }
	drive := filepath.Join(root, "media", "My USB")
	if err := os.MkdirAll(filepath.Join(drive, "Engine Library"), 0700); err != nil {
		t.Fatal(err)
	}
	profile := []byte(`{"data":{"User":{"childorder":["Name","Product","PlayerColor4A","DisplayFileName"],"children":{"Name":{"type":4,"string":"source"},"Product":{"type":4,"string":"JP07"},"PlayerColor4A":{"type":8,"color":"#123456"},"DisplayFileName":{"type":1,"state":true}}}}}`)
	nativePath := filepath.Join(drive, "Engine Library", "user.profile")
	if err := os.WriteFile(nativePath, profile, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.uiDir(), "settings.conf"), []byte("waveform=rgb\nfour_deck_role=13\nscope=spotify\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.exportCopyUSB(drive); err != nil {
		t.Fatal(err)
	}
	// Receiving deck's identity and account selection must be retained.
	if err := os.WriteFile(filepath.Join(s.uiDir(), "settings.conf"), []byte("waveform=denon\nfour_deck_role=24\nscope=deezer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in := copyControl{Drive: drive, Entries: map[string][]string{"DisplayFileName": {}}, Native: map[string]json.RawMessage{"DisplayFileName": json.RawMessage(`{"type":1,"state":false}`)}}
	preview, err := s.copyUSBPreview(in)
	if err != nil || preview["includeEngineXAvailable"] != true {
		t.Fatalf("preview: %v %v", preview, err)
	}
	in.ProfileHash = preview["profileHash"].(string)
	in.Confirmed, in.IncludeUI = true, true
	if err := s.prepareCopyUSB(in); err != nil {
		t.Fatal(err)
	}
	filtered, err := os.ReadFile(filepath.Join(s.copyDir(), "usb.profile"))
	if err != nil || bytes.Contains(filtered, []byte("PlayerColor")) || bytes.Contains(filtered, []byte(`"Name"`)) || bytes.Contains(filtered, []byte(`"Product"`)) {
		t.Fatalf("USB identity filter: %s %v", filtered, err)
	}
	actual, _ := os.ReadFile(nativePath)
	if !bytes.Equal(actual, profile) {
		t.Fatal("USB native profile was overwritten")
	}
	if err := s.finishCopy(copyControl{ID: copyTestOpID(t, s), Native: map[string]json.RawMessage{"DisplayFileName": json.RawMessage(`{"type":1,"state":true}`)}}); err != nil {
		t.Fatal(err)
	}
	s.uiMu.Lock()
	result := s.loadUI()
	s.uiMu.Unlock()
	if restarts != 0 || result.Waveform != "rgb" || result.FourDeckRole != "24" || result.Scope != "deezer" {
		t.Fatalf("USB restart/identity regression: %d %+v", restarts, result)
	}
	if err := os.WriteFile(nativePath, append(profile, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.readCopyUSB(copyControl{Drive: drive, IncludeUI: true}); err == nil {
		t.Fatal("stale companion accepted for a changed native profile")
	}
}

func TestSettingsCopyTwoServersTransferOnlyAfterSourceAllow(t *testing.T) {
	source, _ := copyTestServer(t)
	target, _ := copyTestServer(t)
	sourceHTTP := httptest.NewServer(source)
	defer sourceHTTP.Close()
	portText := sourceHTTP.URL[strings.LastIndex(sourceHTTP.URL, ":")+1:]
	port := 0
	for _, digit := range portText {
		port = port*10 + int(digit-'0')
	}
	target.shadow.peerPort = port
	source.shadow.mu.Lock()
	source.shadow.peers["127.0.0.1"] = time.Now()
	source.shadow.mu.Unlock()
	target.shadow.mu.Lock()
	target.shadow.peers["127.0.0.1"] = time.Now()
	target.shadow.mu.Unlock()
	if err := target.copyBegin("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := target.copyPoll(); err != nil {
		t.Fatal(err)
	}
	if target.copyReceived != nil {
		t.Fatal("target received settings before source Allow")
	}
	source.copyMu.Lock()
	token := source.copyIncoming.Token
	source.copyMu.Unlock()
	body := `{"action":"allow","token":"` + token + `","native":{"DisplayFileName":{"type":1,"state":true}}}`
	w := copyTestRequest(source, "POST", "/v1/sc5000/settings-copy", body, "127.0.0.1:1234", "Bearer TEST-CODE")
	if w.Code != 200 {
		t.Fatalf("source Allow: %d %s", w.Code, w.Body.String())
	}
	if err := target.copyPoll(); err != nil {
		t.Fatal(err)
	}
	if target.copyReceived == nil || len(target.copyReceived.Native) != 1 {
		t.Fatal("approved two-server transfer missing")
	}
	if source.copyIncoming != nil {
		t.Fatal("source grant was not consumed")
	}
	if target.copyOutgoing != nil {
		t.Fatal("receiver kept polling after transfer")
	}
}

func copyTestOpID(t *testing.T, s *Server) string {
	t.Helper()
	var op copyRecord
	if err := s.readCopyJSON("operation.json", &op); err != nil {
		t.Fatal(err)
	}
	return op.ID
}

func TestSettingsCopyExpiryAndSnapshotBinding(t *testing.T) {
	s, _ := copyTestServer(t)
	old := copyRecord{ID: "old", Phase: "usb-load", Expires: time.Now().Add(-time.Second)}
	if err := s.writeCopyJSON("operation.json", old); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCopyJSON("candidate-undo.json", copyUndo{OpID: "older"}); err != nil {
		t.Fatal(err)
	}
	w := copyTestRequest(s, "GET", "/v1/sc5000/settings-copy", "", "127.0.0.1:1", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"phase":"failed"`) || !strings.Contains(w.Body.String(), `"rollback":false`) {
		t.Fatalf("stale USB operation: %s", w.Body.String())
	}
	paused := false
	if err := s.prepareCopyUndo(copyControl{Confirmed: true, Playing: &paused}, true); err == nil {
		t.Fatal("rollback accepted stale snapshot")
	}
	old.Phase = "done"
	if err := s.writeCopyJSON("operation.json", old); err != nil {
		t.Fatal(err)
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", `{"action":"failed","id":"old"}`, "127.0.0.1:1", "")
	if w.Code == 200 {
		t.Fatal("late failure overwrote a finished operation")
	}
}

func TestSettingsCopyScreenActionsRejectRelayAndConsole(t *testing.T) {
	s, _ := copyTestServer(t)
	for _, path := range []string{"/api/settings-copy", "/v1/sc5000/settings-copy"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"action":"usb-export","drive":"/media/test"}`))
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("Authorization", "Bearer TEST-CODE")
		req.Header.Set("Cf-Connecting-Ip", "192.0.2.1")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("relay screen action %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := copyTestRequest(s, "POST", "/api/settings-copy", `{"action":"undo","confirmed":true,"playing":false}`, "127.0.0.1:1", "Bearer TEST-CODE")
	if w.Code != 403 {
		t.Fatal("console performed a screen-only action")
	}
}

func TestSettingsCopyNativeRawSnapshotRestoration(t *testing.T) {
	s, _ := copyTestServer(t)
	paths := copyNativePaths()
	path := filepath.Join(copyNativeDataRoot, paths[0])
	original := []byte("{ \"local\": true }\r\n")
	if err := restoreCopyFile(path, copyFileSnapshot{Present: true, Mode: 0640, Bytes: original}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.copyNativeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(copyNativeDataRoot, paths[1])
	if err := os.WriteFile(absent, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.restoreCopyNative(snap); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if !bytes.Equal(b, original) || info.Mode().Perm() != 0640 {
		t.Fatal("native undo lost bytes or mode")
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("native undo lost absence")
	}
}

func TestSettingsCopyPrestartReentryKeepsOriginalSnapshot(t *testing.T) {
	s, root := copyTestServer(t)
	oldScale := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(root, "ui-scale.env")
	t.Cleanup(func() { uiScaleEnvPath = oldScale })
	path := filepath.Join(s.uiDir(), "settings.conf")
	original := []byte("# preserve my original\r\nwaveform=denon\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	profile, _, err := copyNativeImport(map[string]json.RawMessage{"DisplayFileName": json.RawMessage(`{"type":1,"state":false}`)}, map[string][]string{"DisplayFileName": {}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.copySnapshot(profile, true)
	if err != nil {
		t.Fatal(err)
	}
	snap.OpID = "reentry"
	if err := s.writeCopyJSON("candidate-undo.json", snap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("waveform=rgb\n"), 0600); err != nil {
		t.Fatal(err)
	}
	op := copyRecord{ID: snap.OpID, Phase: "restart", Operation: "copy", Expires: time.Now().Add(time.Minute), Before: profile, After: profile, UI: map[string]json.RawMessage{"waveform": json.RawMessage(`"rgb"`)}}
	if err := s.writeCopyJSON("operation.json", op); err != nil {
		t.Fatal(err)
	}
	w := copyTestRequest(s, "POST", "/v1/sc5000/settings-copy/prestart", `{}`, "127.0.0.1:1", "")
	if w.Code != 200 {
		t.Fatalf("prestart: %d %s", w.Code, w.Body.String())
	}
	var retained copyUndo
	if err := s.readCopyJSON("candidate-undo.json", &retained); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retained.Settings.Bytes, original) {
		t.Fatal("prestart replaced the original snapshot with modified settings")
	}
}

func TestSettingsCopyDenyExpiryAndNoSourceCode(t *testing.T) {
	s, _ := copyTestServer(t)
	token := "11111111111111111111111111111111"
	s.copyIncoming = &copyPeerRequest{Token: token, Peer: "192.0.2.20", Expires: time.Now().Add(time.Minute)}
	body := `{"action":"deny","token":"` + token + `"}`
	w := copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", body, "127.0.0.1:1", "")
	if w.Code == 200 {
		t.Fatal("source consent accepted without its access code")
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", body, "127.0.0.1:1", "Bearer TEST-CODE")
	if w.Code != 200 {
		t.Fatalf("deny: %d %s", w.Code, w.Body.String())
	}
	w = copyTestRequest(s, "GET", "/api/settings-copy/transfer", "", "192.0.2.20:1", "SettingsCopy "+token)
	if w.Code != 403 || strings.Contains(w.Body.String(), "native") {
		t.Fatal("denied source exported settings")
	}
	s.copyIncoming.Expires = time.Now().Add(-time.Second)
	w = copyTestRequest(s, "GET", "/api/settings-copy/transfer", "", "192.0.2.20:1", "SettingsCopy "+token)
	if w.Code != 401 {
		t.Fatal("expired grant accepted")
	}
}

func TestSettingsCopyUndoKeepsNewLocalAccountSettings(t *testing.T) {
	s, root := copyTestServer(t)
	oldScale := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(root, "ui-scale.env")
	t.Cleanup(func() { uiScaleEnvPath = oldScale })
	path := filepath.Join(s.uiDir(), "settings.conf")
	if err := os.WriteFile(path, []byte("scope=deezer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	profile, _, err := copyNativeImport(map[string]json.RawMessage{}, map[string][]string{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.copySnapshot(profile, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeCopyJSON("undo.json", snap); err != nil {
		t.Fatal(err)
	}
	changed := []byte("scope=spotify\n")
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	paused := false
	if err := s.prepareCopyUndo(copyControl{Confirmed: true, Playing: &paused}, false); err == nil {
		t.Fatal("Undo would overwrite a changed local account selection")
	}
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, changed) {
		t.Fatal("Undo refusal changed account settings")
	}
}

func TestSettingsCopyFailedUSBReadCleanupWaitsForNativeLoad(t *testing.T) {
	s, _ := copyTestServer(t)
	op := copyRecord{ID: "failure", Phase: "usb-load", Operation: "usb", Expires: time.Now().Add(time.Minute)}
	if err := s.writeCopyJSON("operation.json", op); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.copyDir(), "usb-read")
	if err := os.WriteFile(marker, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	w := copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", `{"action":"failed","id":"failure","loading":true}`, "127.0.0.1:1", "")
	if w.Code != 200 {
		t.Fatalf("failure: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("in-flight load lost identity filtering")
	}
	w = copyTestRequest(s, "POST", "/v1/sc5000/settings-copy", `{"action":"failed","id":"failure","loading":false}`, "127.0.0.1:1", "")
	if w.Code != 200 {
		t.Fatalf("cleanup: %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("finished failed load retained a stale read marker")
	}
}
