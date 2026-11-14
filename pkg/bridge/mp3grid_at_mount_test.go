package bridge_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func seedEngineXMP3GridAtMount(t *testing.T, opts enginedb.Options) (string, func() float64) {
	t.Helper()
	root := t.TempDir()
	testfixture.Export(t, root)
	if _, err := bridge.ConvertAtMount(context.Background(), root, opts, false); err != nil {
		t.Fatal(err)
	}
	mp3Bytes, err := os.ReadFile(filepath.Join("..", "..", "internal", "audioheader", "testdata", "01-lame-cbr-info-44100.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "MUSIC", "Stream", "pre-r4.mp3")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, mp3Bytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var tb bytes.Buffer
	_ = binary.Write(&tb, binary.BigEndian, 44100.0)
	_ = binary.Write(&tb, binary.BigEndian, uint64(441000))
	_ = binary.Write(&tb, binary.BigEndian, uint32(1))
	for _, pk := range [3]float64{0.68, 0.40, 0.18} {
		_ = binary.Write(&tb, binary.BigEndian, pk)
	}
	td, err := enginedb.QCompress(tb.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var bb bytes.Buffer
	_ = binary.Write(&bb, binary.BigEndian, 44100.0)
	_ = binary.Write(&bb, binary.BigEndian, 441000.0)
	bb.WriteByte(1)
	for range 2 {
		_ = binary.Write(&bb, binary.BigEndian, uint64(2))
		_ = binary.Write(&bb, binary.LittleEndian, 5000.0)
		_ = binary.Write(&bb, binary.LittleEndian, int64(-4))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(24))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(0))
		_ = binary.Write(&bb, binary.LittleEndian, 445000.0)
		_ = binary.Write(&bb, binary.LittleEndian, int64(20))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(0))
		_ = binary.Write(&bb, binary.LittleEndian, uint32(0))
	}
	bb.WriteByte(0)
	bd, err := enginedb.QCompress(bb.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	db := dbOpen(t, root)
	res, err := db.Exec(`INSERT INTO Track(path,filename,fileType,comment,isAnalyzed,isAvailable,isMetadataImported,isBeatGridLocked) VALUES('../MUSIC/Stream/pre-r4.mp3','pre-r4.mp3','mp3','[320k] MP3 44100 Hz; FLAC unavailable',1,1,1,0)`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE PerformanceData SET trackData=?,beatData=? WHERE trackId=?`, td, bd, id); err != nil {
		t.Fatal(err)
	}
	db.Close()

	readBeat0 := func() float64 {
		d := dbOpen(t, root)
		defer d.Close()
		var blob []byte
		if err := d.QueryRow(`SELECT beatData FROM PerformanceData WHERE trackId=?`, id).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		raw, err := enginedb.QUncompress(blob, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(raw[25:33]))
	}
	return root, readBeat0
}

func TestConvertAtMountFixesEngineXMP3Grids(t *testing.T) {
	root, readBeat0 := seedEngineXMP3GridAtMount(t, enginedb.Options{})

	// Next mount hits the rekordbox stamp bypass yet still fixes the EngineX MP3 row.
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || !r.Bypassed {
		t.Fatalf("ConvertAtMount: %+v %v", r, err)
	}

	if got := readBeat0(); math.Abs(got-(5000.0-2257)) > 1e-6 {
		t.Fatalf("beat0=%v, want %v", got, 5000.0-2257)
	}

	// Second mount must not shift again.
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	if got := readBeat0(); math.Abs(got-(5000.0-2257)) > 1e-6 {
		t.Fatalf("beat0 after second mount=%v, want %v", got, 5000.0-2257)
	}
}

// #246: the MP3 grid pass must share the conversion's read deadline.
func TestConvertAtMountMP3GridsShareReadBudget(t *testing.T) {
	root, readBeat0 := seedEngineXMP3GridAtMount(t, enginedb.Options{Fast: true})
	if err := os.Remove(filepath.Join(root, "Engine Library/Database2/.rb-bridge-stamp")); err != nil {
		t.Fatal(err)
	}
	restore := bridge.SlowExportReadForTest(0, time.Nanosecond)
	defer restore()
	r, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false)
	if err != nil || r.Deferred == 0 {
		t.Fatalf("ConvertAtMount: %+v %v", r, err)
	}
	if got := readBeat0(); got != 5000 {
		t.Fatalf("MP3 grid changed after shared read budget exhausted: beat0=%v, want 5000; import deferred=%d", got, r.Deferred)
	}
	restore()
	if _, err := bridge.ConvertAtMount(context.Background(), root, enginedb.Options{}, false); err != nil {
		t.Fatal(err)
	}
	if got := readBeat0(); math.Abs(got-2743) > 1e-6 {
		t.Fatalf("next mount beat0=%v, want 2743", got)
	}
}
