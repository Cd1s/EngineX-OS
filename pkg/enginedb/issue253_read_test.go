package enginedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func readChars(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skip("no /proc/self/io")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "rchar:" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n
		}
	}
	t.Skip("no rchar")
	return 0
}

// #253: reading a track's files was most of an import's time on a USB stick, and most of what it
// read was the .EXT's PWV3, PWV4 and PWV5, which nothing uses once the .2EX has a PWV7 (the
// overview, its peaks and the display all use the PWV7). The import reads their headers only.
func TestIssue253ImportLeavesTheEXTWaveformsUnread(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rchar is Linux's")
	}
	root := t.TempDir()
	testfixture.Export(t, root)
	ext := testfixture.EXT()
	wave := func(tag string, width, count int) []byte {
		b := make([]byte, 12+width*count)
		binary.BigEndian.PutUint32(b, uint32(width))
		binary.BigEndian.PutUint32(b[4:], uint32(count))
		for i := 12; i < len(b); i++ {
			b[i] = byte(i)
		}
		return testfixture.Section(tag, 24, b)
	}
	// The fixture's .EXT with four more megabytes of colour and mono detail.
	big := append(append([]byte(nil), ext...), wave("PWV5", 2, 1<<20)...)
	big = append(big, wave("PWV3", 1, 2<<20)...)
	binary.BigEndian.PutUint32(big[8:], uint32(len(big)))
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.EXT", big)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	before := readChars(t)
	if _, err = Sync(context.Background(), root, l, Options{}); err != nil {
		t.Fatal(err)
	}
	read := readChars(t) - before
	t.Logf("import read %d bytes", read)
	if read > 2<<20 {
		t.Fatalf("the import read %d bytes for an export whose .EXT waveforms are 4 MB", read)
	}
	// What it imported is what the full files give: the cues of the .EXT and the PWV7 overview.
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library/Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cues, overview []byte
	if err = db.QueryRow(`SELECT quickCues,overviewWaveFormData FROM PerformanceData WHERE trackId=1`).Scan(&cues, &overview); err != nil {
		t.Fatal(err)
	}
	// The same export without the extra detail: its overview comes from the same PWV7.
	plain := t.TempDir()
	testfixture.Export(t, plain)
	if _, err = Sync(context.Background(), plain, l, Options{}); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", filepath.Join(plain, "Engine Library/Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var want []byte
	if err = other.QueryRow(`SELECT overviewWaveFormData FROM PerformanceData WHERE trackId=1`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if len(overview) < 100 || !bytes.Equal(overview, want) {
		t.Fatal("the overview is not the one the PWV7 gives")
	}
	if raw, err := QUncompress(cues, 1<<20); err != nil || !bytes.Contains(raw, []byte("H Loop")) {
		t.Fatalf("the .EXT's cues were lost: %v", err)
	}
}
