package webui

import "testing"

// #234: the deck's state dir is recognised under the new path and through the
// /data/sc5000-custom compat link an older unit still passes during an update;
// a test's own dir is not the deck's.
func TestRename234DeckStateDirUnderBothNames(t *testing.T) {
	for dir, want := range map[string]bool{
		"/data/enginex/state":       true,
		"/data/sc5000-custom/state": true,
		"/data/enginex":             false,
		t.TempDir():                 false,
		"":                          false,
	} {
		if got := isDeckStateDir(dir); got != want {
			t.Errorf("isDeckStateDir(%q) = %v, want %v", dir, got, want)
		}
	}
}
