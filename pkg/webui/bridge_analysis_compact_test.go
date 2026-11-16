package webui

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestBridgeDetailsReadCompactRows(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = enginedb.Sync(ctx, root, l, enginedb.Options{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "Engine Library/Database2/m.db")
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rel string
	if err = db.QueryRow(`SELECT path FROM Track WHERE id=1`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	// The fixture's PWV7 has four detail bins at 150 Hz. The source ID, owned
	// path and waveform timebase must survive the new sidecar representation.
	duration := float64(4) * 1000 / 150
	wave := bridgeDisplayWave(ctx, file, rel, 1, 48000, duration)
	if wave == nil || len(wave.Bands) != 12 || wave.SamplesPerBin != 320 {
		t.Fatalf("compact detail not read: %+v", wave)
	}
	for _, tc := range []struct {
		file, rel string
		duration  float64
	}{
		{filepath.Join(filepath.Dir(file), "stm.db"), rel, duration},
		{file, rel + "-wrong", duration}, {file, rel, duration + 200},
	} {
		if bridgeDisplayWave(ctx, tc.file, tc.rel, 1, 48000, tc.duration) != nil {
			t.Fatalf("compact source guard lost: %+v", tc)
		}
	}
}
