package bridge_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// A stick an earlier build imported has overviews without the band maxima Engine scales
// by. The stamp bypass completes them, once.
func TestStampBypassCompletesOldOverviewsOnce(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 1)
	issue233Mount(t, root, enginedb.Options{})
	db := dbOpen(t, root)
	trailer := func() []byte {
		var blob []byte
		if err := db.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE thirdPartySourceId=1`).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		raw, err := enginedb.QUncompress(blob, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return raw[len(raw)-3:]
	}
	asOldBuild := func() {
		var blob []byte
		if err := db.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE thirdPartySourceId=1`).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		raw, _ := enginedb.QUncompress(blob, 1<<20)
		copy(raw[len(raw)-3:], []byte{0, 0, 0})
		blob, _ = enginedb.QCompress(raw)
		if _, err := db.Exec(`UPDATE PerformanceData SET overviewWaveFormData=? WHERE thirdPartySourceId=1`, blob); err != nil {
			t.Fatal(err)
		}
	}
	want := string(trailer())
	if want == "\x00\x00\x00" {
		t.Fatal("fixture overview has no signal")
	}
	asOldBuild()
	if r := issue233Mount(t, root, enginedb.Options{}); !r.Bypassed || string(trailer()) != want {
		t.Fatalf("old overview not completed on the bypass: %+v %v", r, trailer())
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library/Database2/.rb-bridge-overview")); err != nil {
		t.Fatal(err)
	}
	asOldBuild()
	if issue233Mount(t, root, enginedb.Options{}); string(trailer()) != "\x00\x00\x00" {
		t.Fatal("the library was scanned again")
	}
}
