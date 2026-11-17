package main

import (
	"github.com/Cd1s/EngineX-OS/internal/migration"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayKeepsPreviousPublicURLPrefix(t *testing.T) {
	handler := newTestServer(t).routes()
	for _, prefix := range []string{"", "/enginex", "/" + migration.ProjectName} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", prefix+"/healthz", nil))
		if w.Code != 200 {
			t.Errorf("saved public URL prefix %q: HTTP %d", prefix, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/enginexx/healthz", nil))
	if w.Code != 404 {
		t.Fatal("matched an unrelated URL prefix")
	}
}

// #234: a deck without a known LAN address reports its default host name, the legacy placeholder host before
// 5.3.0.5 and enginex.lan since; neither is an address, so neither becomes its ID.
func TestRename234PlaceholderLANNamesAreNotDeckIDs(t *testing.T) {
	for _, name := range []string{migration.PlaceholderHost, "enginex.lan"} {
		ds := newDeckStore(filepath.Join(t.TempDir(), "decks.json"))
		got := ds.upsert(deckInfo{LANIP: name, AccessCode: "ABCD-1234"})
		if got.ID == name || !strings.HasPrefix(got.ID, "deck-") {
			t.Errorf("LAN name %s: ID %q, want the access-code ID", name, got.ID)
		}
	}
}
