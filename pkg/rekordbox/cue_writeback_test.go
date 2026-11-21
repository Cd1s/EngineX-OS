package rekordbox

import (
	"context"
	"database/sql"
	"encoding/binary"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestEncryptSQLCipher4AndCLIProof(t *testing.T) {
	src := filepath.Join("testdata", "sqlcipher4_onelibrary.db")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exportLibrary.db")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	updates := map[uint32][]Cue{
		1: {
			{HotCue: 1, TimeMS: 12500, Name: "Drop A", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 30000, EndMS: 34000, Loop: true, Active: true, Name: "Loop B", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			{HotCue: 0, TimeMS: 5000, Name: "Intro", Color: [4]byte{255, 0x50, 0xb4, 0xff}, ColorCode: 5},
		},
	}
	outPath := filepath.Join(dir, "exportLibrary.new.db")
	if err := UpdateOneLibraryCues(context.Background(), dbPath, updates, outPath); err != nil {
		t.Fatalf("UpdateOneLibraryCues: %v", err)
	}

	// Proof 1: Our own reader reads back the re-encrypted DB.
	lib, err := ReadSQLite(context.Background(), outPath, nil)
	if err != nil {
		t.Fatalf("ReadSQLite: %v", err)
	}
	if len(lib.Tracks) == 0 || len(lib.Tracks[0].Cues) != 3 {
		t.Fatalf("got tracks=%d cues=%d, want 3 cues on track 1", len(lib.Tracks), len(lib.Tracks[0].Cues))
	}

	// Proof 2: sqlcipher CLI opens it with the same key and passes integrity_check.
	if sqlcipherBin, err := exec.LookPath("sqlcipher"); err == nil {
		cmd := exec.Command(sqlcipherBin, outPath,
			fmtSQLCipherPragma(oneLibraryPassphrase),
			"PRAGMA integrity_check;",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sqlcipher integrity_check failed: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "ok") {
			t.Fatalf("unexpected sqlcipher integrity_check output: %q", string(out))
		}
	} else {
		t.Skip("sqlcipher CLI not installed on this host")
	}
}

func fmtSQLCipherPragma(key string) string {
	return "PRAGMA key = '" + strings.ReplaceAll(key, "'", "''") + "';"
}

func TestRewriteANLZCuesRoundTrip(t *testing.T) {
	cues := []Cue{
		{HotCue: 1, TimeMS: 1200, Name: "Intro", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1, ColorID: 1},
		{HotCue: 2, TimeMS: 4500, EndMS: 6500, Loop: true, Active: true, Name: "Chorus", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9, ColorID: 9},
		{HotCue: 0, TimeMS: 500, Name: "Mem1", Color: [4]byte{255, 0x50, 0xb4, 0xff}, ColorCode: 5},
	}

	datOut, changed, err := RewriteANLZCues(testfixture.DAT(), cues, false, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues DAT: changed=%v err=%v", changed, err)
	}
	aDat, err := ParseAnalysis(datOut)
	if err != nil {
		t.Fatalf("ParseAnalysis DAT: %v", err)
	}
	if len(aDat.Cues) != 3 || len(aDat.Beats) == 0 {
		t.Fatalf("DAT cues=%d beats=%d, want 3 cues and >0 beats", len(aDat.Cues), len(aDat.Beats))
	}

	extOut, changed, err := RewriteANLZCues(testfixture.EXT(), cues, true, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
	}
	aExt, err := ParseAnalysis(extOut)
	if err != nil {
		t.Fatalf("ParseAnalysis EXT: %v", err)
	}
	if len(aExt.Cues) != 3 || len(aExt.Waveforms) == 0 {
		t.Fatalf("EXT cues=%d waveforms=%d, want 3 cues and >0 waveforms", len(aExt.Cues), len(aExt.Waveforms))
	}
	var foundChorus bool
	for _, c := range aExt.Cues {
		if c.HotCue == 2 && c.Loop && c.Name == "Chorus" && c.EndMS == 6500 {
			foundChorus = true
		}
	}
	if !foundChorus {
		t.Fatalf("missing Chorus hot loop in EXT: %+v", aExt.Cues)
	}
}

func TestUpdateOneLibraryCuesNoTempLeakOnError(t *testing.T) {
	// Encrypt a valid SQLite DB that lacks the cue table so UpdateOneLibraryCues
	// decrypts to /tmp and then fails inside applyCueUpdatesSQLite.
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "bad-plain.db")
	// Use decrypted fixture and drop the cue table so reserveSize=80 is preserved.
	rawPlain, err := os.ReadFile(filepath.Join("testdata", "sqlcipher4_onelibrary_decrypted.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, rawPlain, 0o600); err != nil {
		t.Fatal(err)
	}
	salt, err := ReadSQLCipherSalt(filepath.Join("testdata", "sqlcipher4_onelibrary.db"))
	if err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dir, "bad-enc.db")
	// Drop the cue table using Go's sql driver while keeping reserveSize=80.
	if err := dropCueTableForTest(plainPath); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := in.Stat()
	if err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	out, err := os.Create(encPath)
	if err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	encErr := EncryptSQLCipher4(context.Background(), in, st.Size(), out, salt, []byte(oneLibraryPassphrase))
	_ = in.Close()
	_ = out.Close()
	if encErr != nil {
		t.Fatal(encErr)
	}

	tempsBefore := listTempOneLibraryFiles(t)
	err = UpdateOneLibraryCues(context.Background(), encPath, map[uint32][]Cue{
		1: {{HotCue: 1, TimeMS: 1000}},
	}, filepath.Join(dir, "out.db"))
	if err == nil {
		t.Fatal("expected error when cue table is missing")
	}
	for p := range listTempOneLibraryFiles(t) {
		if !tempsBefore[p] {
			t.Fatalf("UpdateOneLibraryCues leaked temporary decrypted file on error: %s", p)
		}
	}
}

func dropCueTableForTest(plainPath string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: plainPath}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`PRAGMA journal_mode=DELETE; DROP TABLE cue;`)
	return err
}

func TestRewriteANLZCuesMatchesRekordboxFormat(t *testing.T) {
	cues := []Cue{
		{HotCue: 1, TimeMS: 1200, Name: "Intro", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
		{HotCue: 2, TimeMS: 4500, EndMS: 6500, Loop: true, Active: true, LoopNumerator: 4, LoopDenominator: 1, Name: "Chorus", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
		{HotCue: 0, TimeMS: 500, Name: "Mem1", ColorID: 5},
	}

	inspectSections := func(raw []byte) (pcobKinds, pco2Kinds []uint32) {
		h, n := int(binary.BigEndian.Uint32(raw[4:])), int(binary.BigEndian.Uint32(raw[8:]))
		for off := h; off < n; {
			tag := string(raw[off : off+4])
			sn := int(binary.BigEndian.Uint32(raw[off+8:]))
			sec := raw[off : off+sn]
			switch tag {
			case "PCOB":
				kind := binary.BigEndian.Uint32(sec[12:])
				unk := binary.BigEndian.Uint16(sec[16:])
				count := binary.BigEndian.Uint16(sec[18:])
				memCount := binary.BigEndian.Uint32(sec[20:])
				pcobKinds = append(pcobKinds, kind)
				if unk != 0 {
					t.Errorf("PCOB(kind=%d) unk=0x%04x, want 0x0000", kind, unk)
				}
				if kind == 1 && memCount != 0xffffffff {
					t.Errorf("PCOB(kind=1) memory_count=0x%08x, want 0xffffffff", memCount)
				}
				poff := 24
				for i := 0; i < int(count); i++ {
					psn := int(binary.BigEndian.Uint32(sec[poff+8:]))
					p := sec[poff : poff+psn]
					u1 := binary.BigEndian.Uint32(p[20:])
					if u1 != 0x10000 {
						t.Errorf("PCPT[%d] u1=0x%x, want 0x10000", i, u1)
					}
					poff += psn
				}
			case "PCO2":
				kind := binary.BigEndian.Uint32(sec[12:])
				count := binary.BigEndian.Uint16(sec[16:])
				pco2Kinds = append(pco2Kinds, kind)
				poff := 20
				for i := 0; i < int(count); i++ {
					psn := int(binary.BigEndian.Uint32(sec[poff+8:]))
					p := sec[poff : poff+psn]
					hc := binary.BigEndian.Uint32(p[12:])
					colorID := p[28]
					clen := int(binary.BigEndian.Uint32(p[40:]))
					q := 44 + clen
					colorCode := p[q]
					if hc > 0 {
						if colorID != 0 {
							t.Errorf("PCP2 hot cue %d color_id=%d, want 0", hc, colorID)
						}
						if colorCode == 0 {
							t.Errorf("PCP2 hot cue %d color_code=0, want non-zero palette code", hc)
						}
					} else {
						if colorID != 5 {
							t.Errorf("PCP2 memory cue color_id=%d, want 5", colorID)
						}
						if colorCode != 0 || p[q+1] != 0 || p[q+2] != 0 || p[q+3] != 0 {
							t.Errorf("PCP2 memory cue hot-cue color bytes=%x, want 00000000", p[q:q+4])
						}
					}
					poff += psn
				}
			}
			off += sn
		}
		return pcobKinds, pco2Kinds
	}

	datOut, changed, err := RewriteANLZCues(testfixture.DAT(), cues, false, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues DAT: changed=%v err=%v", changed, err)
	}
	datPCOB, datPCO2 := inspectSections(datOut)
	if len(datPCOB) != 2 || len(datPCO2) != 0 {
		t.Fatalf("DAT sections: PCOB=%v PCO2=%v, want 2 PCOB and 0 PCO2", datPCOB, datPCO2)
	}

	extOut, changed, err := RewriteANLZCues(testfixture.EXT(), cues, true, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
	}
	extPCOB, extPCO2 := inspectSections(extOut)
	if len(extPCOB) != 2 || len(extPCO2) != 2 {
		t.Fatalf("EXT sections: PCOB=%v PCO2=%v, want 2 PCOB and 2 PCO2 (rekordbox EXT format)", extPCOB, extPCO2)
	}
	aExt, err := ParseAnalysis(extOut)
	if err != nil {
		t.Fatalf("ParseAnalysis EXT: %v", err)
	}
	var sawActiveLoop, sawMem bool
	for _, c := range aExt.Cues {
		if c.HotCue == 2 && c.Loop && c.Active && c.Name == "Chorus" && c.ColorCode == 9 {
			sawActiveLoop = true
		}
		if c.HotCue == 0 && c.TimeMS == 500 && c.ColorID == 5 && c.ColorCode == 0 && c.Color == ([4]byte{}) {
			sawMem = true
		}
	}
	if !sawActiveLoop {
		t.Fatalf("EXT lost Active flag on Hot Loop B: %+v", aExt.Cues)
	}
	if !sawMem {
		t.Fatalf("EXT memory cue color round-trip mismatch: %+v", aExt.Cues)
	}
}

func TestUpdateOneLibraryCuesSchemaAndChangeTracking(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exportLibrary.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	// Exact cue + content columns from a real rekordbox 7 exportLibrary.db.
	_, err = db.Exec(`
		PRAGMA journal_mode=DELETE;
		CREATE TABLE content(
			content_id integer primary key,
			title varchar,
			bpmx100 integer,
			length integer,
			path varchar,
			samplingRate integer,
			djComment varchar,
			analysisDataFilePath varchar,
			hasModified integer,
			cueUpdateCount integer
		);
		CREATE TABLE cue(
			cue_id integer primary key,
			content_id integer,
			kind integer,
			colorTableIndex integer,
			cueComment varchar,
			isActiveLoop integer,
			beatLoopNumerator integer,
			beatLoopDenominator integer,
			inUsec integer,
			outUsec integer,
			in150FramePerSec integer,
			out150FramePerSec integer
		);
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, hasModified, cueUpdateCount)
		VALUES (1, 'Track 1', 12800, 180, '/Contents/t1.mp3', 44100, '', '/.PIONEER/USBANLZ/P001/ANLZ0000.DAT', 0, NULL);
	`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	updates := map[uint32][]Cue{
		1: {
			{HotCue: 1, TimeMS: 1200, Name: "Drop A", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 2, TimeMS: 4500, EndMS: 6500, Loop: true, Active: true, LoopNumerator: 4, LoopDenominator: 1, Name: "Loop B", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			{HotCue: 0, TimeMS: 500, Name: "Mem Red", ColorID: 2},
		},
	}
	out1 := filepath.Join(dir, "out1.db")
	if err := UpdateOneLibraryCues(context.Background(), dbPath, updates, out1); err != nil {
		t.Fatalf("UpdateOneLibraryCues #1: %v", err)
	}
	out2 := filepath.Join(dir, "out2.db")
	if err := UpdateOneLibraryCues(context.Background(), out1, updates, out2); err != nil {
		t.Fatalf("UpdateOneLibraryCues #2: %v", err)
	}

	checkDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: out2}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()

	var hasMod, cueUpd int
	if err := checkDB.QueryRow(`SELECT hasModified, cueUpdateCount FROM content WHERE content_id = 1`).Scan(&hasMod, &cueUpd); err != nil {
		t.Fatalf("query content change tracking: %v", err)
	}
	if hasMod != 1 || cueUpd != 2 {
		t.Fatalf("content hasModified=%d cueUpdateCount=%d, want 1 and 2", hasMod, cueUpd)
	}

	rows, err := checkDB.Query(`SELECT kind, colorTableIndex, isActiveLoop, inUsec, outUsec, in150FramePerSec, out150FramePerSec FROM cue WHERE content_id = 1 ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type cueRow struct {
		kind, colorIdx, active int
		inUsec, outUsec        int64
		in150, out150          sql.NullInt64
	}
	var got []cueRow
	for rows.Next() {
		var r cueRow
		if err := rows.Scan(&r.kind, &r.colorIdx, &r.active, &r.inUsec, &r.outUsec, &r.in150, &r.out150); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("got %d cue rows, want 3", len(got))
	}
	// kind=0 memory cue: colorTableIndex=2 (Red), outUsec=-1, in150=75, out150=-1
	if got[0].kind != 0 || got[0].colorIdx != 2 || got[0].outUsec != -1 || !got[0].in150.Valid || got[0].in150.Int64 != 75 || !got[0].out150.Valid || got[0].out150.Int64 != -1 {
		t.Fatalf("unexpected memory cue row: %+v", got[0])
	}
	// kind=1 hot cue point: colorTableIndex=1, outUsec=-1, in150=180, out150=-1
	if got[1].kind != 1 || got[1].colorIdx != 1 || got[1].outUsec != -1 || !got[1].in150.Valid || got[1].in150.Int64 != 180 || !got[1].out150.Valid || got[1].out150.Int64 != -1 {
		t.Fatalf("unexpected hot cue row: %+v", got[1])
	}
	// kind=2 hot active loop: colorTableIndex=9, active=1, outUsec=6500000, in150=675, out150=975
	if got[2].kind != 2 || got[2].colorIdx != 9 || got[2].active != 1 || got[2].outUsec != 6500000 || !got[2].in150.Valid || got[2].in150.Int64 != 675 || !got[2].out150.Valid || got[2].out150.Int64 != 975 {
		t.Fatalf("unexpected hot loop row: %+v", got[2])
	}
}
