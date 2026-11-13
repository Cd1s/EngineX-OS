// Package release compares EngineX OS versions and release tags, for the deck's updater and
// EngineX-Installer alike.
package release

import (
	"regexp"
	"strings"
)

var numberRE = regexp.MustCompile(`\d+`)

// Newer compares the numbers in two versions or tags in order, so v5.0.4-custom.10 is newer
// than v5.0.4-custom.9 and v5.3.0.4-r2 newer than 5.3.0.4. A side without numbers ("dev", no
// version.txt) is unknown: never newer, never older.
func Newer(latest, current string) bool {
	l, c := numberRE.FindAllString(latest, -1), numberRE.FindAllString(current, -1)
	if len(l) == 0 || len(c) == 0 {
		return false
	}
	for i := 0; i < len(l) && i < len(c); i++ {
		a, b := strings.TrimLeft(l[i], "0"), strings.TrimLeft(c[i], "0")
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		if a != b {
			return a > b
		}
	}
	return len(l) > len(c)
}

// Of is what a release is compared by: its tag when that belongs to its version (a re-release,
// v5.3.0.4-r4), else the version.
func Of(version, tag string) string {
	version, tag = strings.TrimSpace(version), strings.TrimSpace(tag)
	t, v := numberRE.FindAllString(tag, -1), numberRE.FindAllString(version, -1)
	if len(v) == 0 || len(t) < len(v) {
		return version
	}
	for i := range v {
		if strings.TrimLeft(t[i], "0") != strings.TrimLeft(v[i], "0") {
			return version
		}
	}
	return tag
}

// Older reports whether the target release is older than the installed one (#234): then the
// installed release leaves (its own units out of /run) and the deck reboots, instead of running
// the older release's activation in place. Unknown versions are never older.
func Older(installedVersion, installedTag, targetVersion, targetTag string) bool {
	return Newer(Of(installedVersion, installedTag), Of(targetVersion, targetTag))
}
