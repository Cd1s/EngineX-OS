package release

import "testing"

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"5.3.0.5", "5.3.0.4", true}, {"v5.3.0.4-r4", "5.3.0.4", true}, {"v5.3.0.5", "v5.3.0.4-r5", true},
		{"5.3.0.4", "5.3.0.5", false}, {"5.3.0.5", "5.3.0.5", false}, {"v5.0.4-custom.10", "v5.0.4-custom.9", true},
		{"dev", "5.3.0.5", false}, {"5.3.0.5", "dev", false}, {"5.3.0.10", "5.3.0.9", true},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// #234: what counts as a downgrade, by version and, within one version, by release tag:
// 5.3.0.5 -> r5 and a bundle without a tag leave and reboot, an earlier re-release of the same
// version too; an upgrade or the same release activates in place; a tag of another version is
// ignored; unknown versions are never older.
func TestOlder(t *testing.T) {
	for _, c := range []struct {
		iv, it, tv, tt string
		want           bool
	}{
		{"5.3.0.5", "v5.3.0.5", "5.3.0.4", "v5.3.0.4-r5", true},
		{"5.3.0.5", "", "5.3.0.3", "", true},
		{"5.3.0.5", "v5.3.0.5-r2", "5.3.0.5", "v5.3.0.5-r1", true},
		{"5.3.0.5", "v5.3.0.5-r1", "5.3.0.5", "v5.3.0.5-r1", false},
		{"5.3.0.5", "v5.3.0.5", "5.3.0.6", "v5.3.0.6", false},
		{"5.3.0.4", "v5.3.0.4-r4", "5.3.0.5", "", false},
		{"5.3.0.5", "v5.3.0.4-r4", "5.3.0.5", "", false},
		{"dev", "", "5.3.0.4", "", false}, {"5.3.0.5", "", "", "", false},
	} {
		if got := Older(c.iv, c.it, c.tv, c.tt); got != c.want {
			t.Errorf("Older(%q %q -> %q %q) = %v", c.iv, c.it, c.tv, c.tt, got)
		}
	}
}
