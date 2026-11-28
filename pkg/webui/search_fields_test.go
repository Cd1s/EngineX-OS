package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func uiGet(t *testing.T, base string) (int, uiSettings) {
	t.Helper()
	resp, err := http.Get(base + "/v1/enginex/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v uiSettings
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, v
}

func uiPostSeq(t *testing.T, base, query, clientID string, seq int) (int, http.Header, uiSettings) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/enginex/ui?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clientID != "" {
		req.Header.Set("X-UI-Client-Id", clientID)
		req.Header.Set("X-UI-Write-Seq", strconv.Itoa(seq))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v uiSettings
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, resp.Header.Clone(), v
}

func TestUISearchFieldsDefaultAndPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	s1 := New(cfg, nil)
	defer s1.Close()
	srv := httptest.NewServer(s1)
	defer srv.Close()

	wantDefault := []string{"song", "artist"}
	if code, v := uiGet(t, srv.URL); code != 200 || !slices.Equal(v.SearchFields, wantDefault) {
		t.Fatalf("default: status %d, searchFields %v, want %v", code, v.SearchFields, wantDefault)
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.conf")); err == nil {
		t.Fatalf("default GET unexpectedly created settings.conf")
	}

	// Out-of-order + duplicate roles canonicalize into Search.qml menuRoles order.
	wantCustom := []string{"song", "key", "comment", "bpm"}
	if code, v := uiPost(t, srv.URL, "searchFields=bpm,key,song,comment,song"); code != 200 || !slices.Equal(v.SearchFields, wantCustom) {
		t.Fatalf("custom save: status %d, searchFields %v, want %v", code, v.SearchFields, wantCustom)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil || !slices.Contains(splitLines(data), "search_fields=song,key,comment,bpm") {
		t.Fatalf("settings.conf missing search_fields: %q %v", data, err)
	}

	// Survives server restart and unrelated saves.
	s2 := New(cfg, nil)
	defer s2.Close()
	again := httptest.NewServer(s2)
	defer again.Close()
	if code, v := uiGet(t, again.URL); code != 200 || !slices.Equal(v.SearchFields, wantCustom) {
		t.Fatalf("after restart: status %d, searchFields %v, want %v", code, v.SearchFields, wantCustom)
	}
	if code, v := uiPost(t, again.URL, "waveform=rgb"); code != 200 || !slices.Equal(v.SearchFields, wantCustom) {
		t.Fatalf("after unrelated save: status %d, searchFields %v, want %v", code, v.SearchFields, wantCustom)
	}
	data, err = os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil || !slices.Contains(splitLines(data), "search_fields=song,key,comment,bpm") {
		t.Fatalf("unrelated save dropped search_fields: %q %v", data, err)
	}

	// Snake-case parameter and all 12 roles round-trip.
	all12 := "filename,dateImported,year,label,genre,bpm,comment,key,length,album,artist,song"
	wantAll12 := []string{"song", "artist", "album", "length", "key", "comment", "bpm", "genre", "label", "year", "dateImported", "filename"}
	if code, v := uiPost(t, again.URL, "search_fields="+all12); code != 200 || !slices.Equal(v.SearchFields, wantAll12) {
		t.Fatalf("all 12 roles: status %d, searchFields %v, want %v", code, v.SearchFields, wantAll12)
	}

	// JSON POST body works too.
	req, err := http.NewRequest("POST", again.URL+"/v1/enginex/ui", bytes.NewBufferString(`{"searchFields":"genre,label"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var jsonOut uiSettings
	if err := json.NewDecoder(resp.Body).Decode(&jsonOut); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !slices.Equal(jsonOut.SearchFields, []string{"genre", "label"}) {
		t.Fatalf("JSON update: status %d, searchFields %v", resp.StatusCode, jsonOut.SearchFields)
	}

	// Returning to stock embedded defaults omits search_fields= from settings.conf.
	if code, v := uiPost(t, again.URL, "searchFields=artist,song"); code != 200 || !slices.Equal(v.SearchFields, wantDefault) {
		t.Fatalf("reset to stock default: status %d, searchFields %v", code, v.SearchFields)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "settings.conf"))
	for _, line := range splitLines(data) {
		if strings.HasPrefix(line, "search_fields=") {
			t.Fatalf("default search_fields should be omitted from settings.conf: %q", data)
		}
	}

	// Empty query value resets to stock defaults too.
	if code, _ := uiPost(t, again.URL, "searchFields=bpm"); code != 200 {
		t.Fatalf("set bpm failed: %d", code)
	}
	if code, v := uiPost(t, again.URL, "searchFields="); code != 200 || !slices.Equal(v.SearchFields, wantDefault) {
		t.Fatalf("empty reset: status %d, searchFields %v", code, v.SearchFields)
	}
}

func TestUISearchFieldsRejectsInvalidAndRecoversCorruptFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{AccessToken: "code", UIDir: dir, StateDir: t.TempDir()}
	s := New(cfg, nil)
	defer s.Close()
	srv := httptest.NewServer(s)
	defer srv.Close()

	for _, q := range []string{"searchFields=bogus", "searchFields=song,evil", "searchFields=,", "searchFields=,,", "search_fields=../etc"} {
		code, _, _ := uiPostSeq(t, srv.URL, q, "", 0)
		if code != 400 {
			t.Errorf("%s: status %d, want 400", q, code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.conf")); err == nil {
		t.Fatalf("invalid searchFields wrote settings.conf")
	}

	// Corrupt settings.conf keeps valid roles and ignores bogus tokens.
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("search_fields=bogus,key,bad,bpm,key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiGet(t, srv.URL); code != 200 || !slices.Equal(v.SearchFields, []string{"key", "bpm"}) {
		t.Fatalf("partial corrupt recovery: status %d, searchFields %v", code, v.SearchFields)
	}

	// Totally invalid settings.conf falls back to stock defaults.
	if err := os.WriteFile(filepath.Join(dir, "settings.conf"), []byte("search_fields=totally_invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, v := uiGet(t, srv.URL); code != 200 || !slices.Equal(v.SearchFields, []string{"song", "artist"}) {
		t.Fatalf("full corrupt fallback: status %d, searchFields %v", code, v.SearchFields)
	}
}

// TestS23DelayedSaveSurvivesRestart reproduces S23 R1 BLOCK 1: an older write (seq=1, album)
// held before the handler while a newer write (seq=2, album+bpm) commits must be rejected with
// 409 Conflict so it can never overwrite settings.conf across a server restart.
func TestS23DelayedSaveSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{UIDir: dir, StateDir: t.TempDir()}
	handler := New(cfg, nil)
	defer handler.Close()

	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("searchFields") == "album" {
			close(arrived)
			<-release
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	type result struct {
		code int
		hdr  http.Header
	}
	done := make(chan result, 1)
	go func() {
		code, hdr, _ := uiPostSeq(t, srv.URL, "searchFields=album", "qml-s23", 1)
		done <- result{code: code, hdr: hdr}
	}()
	<-arrived

	newerCode, _, newerUI := uiPostSeq(t, srv.URL, "searchFields=album,bpm", "qml-s23", 2)
	close(release)
	earlier := <-done

	if newerCode != http.StatusOK || !slices.Equal(newerUI.SearchFields, []string{"album", "bpm"}) {
		t.Fatalf("newer write: status=%d fields=%v, want 200 [album bpm]", newerCode, newerUI.SearchFields)
	}
	if earlier.code != http.StatusConflict {
		t.Fatalf("delayed earlier write: status=%d, want 409 Conflict (newer=%d)", earlier.code, newerCode)
	}

	disk, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(splitLines(disk), "search_fields=album,bpm") {
		t.Fatalf("settings.conf lost newer selection: %q", disk)
	}

	restarted := New(cfg, nil)
	defer restarted.Close()
	got := restarted.loadUI().SearchFields
	if !slices.Equal(got, []string{"album", "bpm"}) {
		t.Fatalf("after restart got %v, want [album bpm]", got)
	}
}

// TestS23SearchFieldsRevEpochAndErrorHeaders verifies E4 + S23 composition: searchFields writes
// advance persisted rev within an epoch, invalid writes return 400 with rev/epoch metadata, and
// restarting the server advances to a new epoch + rev while retaining search_fields.
func TestS23SearchFieldsRevEpochAndErrorHeaders(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{UIDir: dir, StateDir: t.TempDir()}
	s1 := New(cfg, nil)
	defer s1.Close()
	srv := httptest.NewServer(s1)
	defer srv.Close()

	code, hdr, bad := uiPostSeq(t, srv.URL, "searchFields=bogus", "", 0)
	if code != http.StatusBadRequest || bad.Rev <= 0 || bad.Epoch == "" || hdr.Get("X-UI-Rev") == "" || hdr.Get("X-UI-Epoch") == "" {
		t.Fatalf("400 missing E4 rev/epoch metadata: code=%d body=(%d,%q) hdr=(%q,%q)",
			code, bad.Rev, bad.Epoch, hdr.Get("X-UI-Rev"), hdr.Get("X-UI-Epoch"))
	}

	okCode, okHdr, okUI := uiPostSeq(t, srv.URL, "searchFields=bpm,key", "qml-s23-rev", 2)
	if okCode != http.StatusOK || !slices.Equal(okUI.SearchFields, []string{"key", "bpm"}) || okUI.Rev <= bad.Rev || okUI.Epoch != bad.Epoch {
		t.Fatalf("save rev/epoch mismatch: code=%d fields=%v rev=%d->%d epoch=%q->%q",
			okCode, okUI.SearchFields, bad.Rev, okUI.Rev, bad.Epoch, okUI.Epoch)
	}
	if okHdr.Get("X-UI-Rev") != strconv.FormatInt(okUI.Rev, 10) || okHdr.Get("X-UI-Epoch") != okUI.Epoch {
		t.Fatalf("header/body rev/epoch mismatch: hdr=(%q,%q) body=(%d,%q)",
			okHdr.Get("X-UI-Rev"), okHdr.Get("X-UI-Epoch"), okUI.Rev, okUI.Epoch)
	}

	staleCode, staleHdr, staleBody := uiPostSeq(t, srv.URL, "searchFields=album", "qml-s23-rev", 1)
	if staleCode != http.StatusConflict || staleBody.Rev != okUI.Rev || staleBody.Epoch != okUI.Epoch || staleHdr.Get("X-UI-Rev") == "" || staleHdr.Get("X-UI-Epoch") == "" {
		t.Fatalf("409 stale write metadata mismatch: code=%d body=(%d,%q) hdr=(%q,%q)",
			staleCode, staleBody.Rev, staleBody.Epoch, staleHdr.Get("X-UI-Rev"), staleHdr.Get("X-UI-Epoch"))
	}

	s2 := New(cfg, nil)
	defer s2.Close()
	if got := s2.loadUI().SearchFields; !slices.Equal(got, []string{"key", "bpm"}) {
		t.Fatalf("after restart got %v, want [key bpm]", got)
	}
	if s2.currentUIEpoch() == okUI.Epoch || s2.currentUIRev() <= okUI.Rev {
		t.Fatalf("restart epoch/rev not advanced: before=(%q,%d) after=(%q,%d)",
			okUI.Epoch, okUI.Rev, s2.currentUIEpoch(), s2.currentUIRev())
	}
}

// TestS23DelayedSaveCannotUndoResetAcrossRestart ensures a delayed pre-reset write (seq=1)
// released after Reset All (seq=2 -> song,artist) returns 409 and never puts search_fields=
// back into settings.conf across a server restart.
func TestS23DelayedSaveCannotUndoResetAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{UIDir: dir, StateDir: t.TempDir()}
	handler := New(cfg, nil)
	defer handler.Close()

	arrived, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("searchFields") == "album,bpm" {
			close(arrived)
			<-release
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		code, _, _ := uiPostSeq(t, srv.URL, "searchFields=album,bpm", "qml-s23-reset", 1)
		done <- code
	}()
	<-arrived

	resetCode, _, resetUI := uiPostSeq(t, srv.URL, "searchFields=song,artist", "qml-s23-reset", 2)
	close(release)
	staleCode := <-done

	if resetCode != http.StatusOK || !slices.Equal(resetUI.SearchFields, []string{"song", "artist"}) {
		t.Fatalf("reset write: status=%d fields=%v", resetCode, resetUI.SearchFields)
	}
	if staleCode != http.StatusConflict {
		t.Fatalf("delayed pre-reset write: status=%d, want 409 Conflict", staleCode)
	}
	disk, err := os.ReadFile(filepath.Join(dir, "settings.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range splitLines(disk) {
		if strings.HasPrefix(line, "search_fields=") {
			t.Fatalf("delayed pre-reset write resurrected %q in settings.conf", line)
		}
	}

	restarted := New(cfg, nil)
	defer restarted.Close()
	if got := restarted.loadUI().SearchFields; !slices.Equal(got, []string{"song", "artist"}) {
		t.Fatalf("after restart got %v, want [song artist]", got)
	}
}
