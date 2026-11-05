package cli

import "testing"

func TestStageName(t *testing.T) {
	for in, want := range map[string]string{
		"CDJ-3000":                  "CDJ-3000 2 USB",
		"../../etc\x00":             "etc 2 USB",
		"":                          "Player 2 USB",
		"a very long player name x": "a very long player name 2 USB",
	} {
		if got := stageName(in, 2, 3); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := stageName("XDJ", 1, 2); got != "XDJ 1 SD" {
		t.Error(got)
	}
}
