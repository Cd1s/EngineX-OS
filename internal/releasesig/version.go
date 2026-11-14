package releasesig

import (
	"regexp"
	"strings"
)

// The deck's version comparison, shared with the installer so both decide "older" the same way.

var versionNumberRE = regexp.MustCompile(`\d+`)

// NewerVersion compares the numbers in two tags in order, so v5.0.4-custom.10 is
// newer than v5.0.4-custom.9 and an older tag is never offered. A current
// version without numbers ("dev", no version.txt) is unknown: never claim an update.
func NewerVersion(latest, current string) bool {
	l, c := versionNumberRE.FindAllString(latest, -1), versionNumberRE.FindAllString(current, -1)
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

// TagOfVersion reports whether tag starts with the numbers of version.
func TagOfVersion(tag, version string) bool {
	t, v := versionNumberRE.FindAllString(tag, -1), versionNumberRE.FindAllString(version, -1)
	if len(v) == 0 || len(t) < len(v) {
		return false
	}
	for i := range v {
		if strings.TrimLeft(t[i], "0") != strings.TrimLeft(v[i], "0") {
			return false
		}
	}
	return true
}

// InstalledRelease is what a release tag is compared with: the newest tag of the installed
// version that the installed image or bundle carries, else the version. A same-version re-release
// (v5.3.0.4-r2) is newer than version 5.3.0.4 and must not be offered to itself. A tag of another
// version (a bundle downgraded by hand) is ignored.
func InstalledRelease(version string, tags []string) string {
	release := version
	for _, tag := range tags {
		if TagOfVersion(tag, version) && NewerVersion(tag, release) {
			release = tag
		}
	}
	return release
}
