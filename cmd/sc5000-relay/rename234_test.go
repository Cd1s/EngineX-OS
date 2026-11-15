package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// #234: a deck without a known LAN address reports its default host name, sc5000.lan before
// 5.3.0.5 and enginex.lan since; neither is an address, so neither becomes its ID.
func TestRename234PlaceholderLANNamesAreNotDeckIDs(t *testing.T) {
	for _, name := range []string{"sc5000.lan", "enginex.lan"} {
		ds := newDeckStore(filepath.Join(t.TempDir(), "decks.json"))
		got := ds.upsert(deckInfo{LANIP: name, AccessCode: "ABCD-1234"})
		if got.ID == name || !strings.HasPrefix(got.ID, "deck-") {
			t.Errorf("LAN name %s: ID %q, want the access-code ID", name, got.ID)
		}
	}
}
