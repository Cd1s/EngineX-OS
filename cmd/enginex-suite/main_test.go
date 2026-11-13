package main

import (
	"reflect"
	"testing"
)

// #234: the suite answers to its new names and, during an update from an older
// release, to the old ones its still-loaded units and wrappers use.
func TestAppletAcceptsNewAndOldNames(t *testing.T) {
	for _, c := range []struct {
		prog, want string
		args, rest []string
	}{
		{"enginex-suite", "stream-sync", []string{"stream-sync", "--listen", ":8080"}, []string{"--listen", ":8080"}},
		{"sc5000-suite", "stream-sync", []string{"stream-sync", "--listen", ":8080"}, []string{"--listen", ":8080"}},
		{"enginex-suite", "enginex-deemixd", []string{"sc5000-deemixd", "-state-dir", "/x"}, []string{"-state-dir", "/x"}},
		{"sc5000-suite", "enginex-deemixd", []string{"enginex-deemixd"}, []string{}},
		{"enginex-deemixd", "enginex-deemixd", []string{"-state-dir", "/x"}, []string{"-state-dir", "/x"}},
		{"sc5000-deemixd", "enginex-deemixd", []string{"-state-dir", "/x"}, []string{"-state-dir", "/x"}},
		{"rb-bridge", "rb-bridge", []string{"--prolink"}, []string{"--prolink"}},
		{"enginex-suite", "enginex-suite", nil, nil},
	} {
		got, rest := applet(c.prog, c.args)
		if got != c.want || !reflect.DeepEqual(rest, c.rest) && (len(rest) != 0 || len(c.rest) != 0) {
			t.Errorf("applet(%q, %q) = %q, %q; want %q, %q", c.prog, c.args, got, rest, c.want, c.rest)
		}
	}
}
