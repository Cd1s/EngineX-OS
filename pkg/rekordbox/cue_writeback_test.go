package rekordbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

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
				if kind == 0 {
					wantMem := uint32(0xffffffff)
					if count > 0 {
						wantMem = uint32(count - 1)
					}
					if memCount != wantMem {
						t.Errorf("PCOB(kind=0, count=%d) memory_count=0x%08x, want 0x%08x", count, memCount, wantMem)
					}
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
					b29 := p[29]
					clen := int(binary.BigEndian.Uint32(p[40:]))
					if b29 != 0x01 || psn != 88+clen {
						t.Errorf("PCP2 full entry b29=0x%02x psn=%d, want 0x01 and %d", b29, psn, 88+clen)
					}
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
	if len(datPCOB) != 2 || datPCOB[0] != 1 || datPCOB[1] != 0 || len(datPCO2) != 0 {
		t.Fatalf("DAT sections: PCOB=%v PCO2=%v, want PCOB=[1 0] and 0 PCO2", datPCOB, datPCO2)
	}

	extOut, changed, err := RewriteANLZCues(testfixture.EXT(), cues, true, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
	}
	extPCOB, extPCO2 := inspectSections(extOut)
	if len(extPCOB) != 2 || extPCOB[0] != 1 || extPCOB[1] != 0 || len(extPCO2) != 2 || extPCO2[0] != 1 || extPCO2[1] != 0 {
		t.Fatalf("EXT sections: PCOB=%v PCO2=%v, want PCOB=[1 0] and PCO2=[1 0] (rekordbox EXT format)", extPCOB, extPCO2)
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

// indepANLZSection and parseANLZIndependent implement an independent Kaitai/rekordcrate-spec
// binary parser (Deep Symmetry rekordbox_anlz.ksy + Holzhaus/rekordcrate src/anlz.rs) so tests
// validate our ANLZ output independently of ParseAnalysis.
type indepANLZSection struct {
	tag  string
	hdr  int
	raw  []byte
	kind uint32
}

type indepPCPT struct {
	hotCue     uint32
	status     uint32
	u1         uint32
	orderFirst uint16
	orderLast  uint16
	cueType    byte
	timeMS     uint32
	loopTimeMS uint32
}

type indepPCP2 struct {
	lenEntry   int
	hotCue     uint32
	cueType    byte
	timeMS     uint32
	loopTimeMS uint32
	colorID    byte
	b29        byte
	loopNum    uint16
	loopDen    uint16
	comment    string
	hasTailRGB bool
	colorCode  byte
	rgb        [3]byte
}

type indepPCOB struct {
	kind     uint32
	memCount uint32
	entries  []indepPCPT
}

type indepPCO2 struct {
	kind    uint32
	entries []indepPCP2
}

type indepPQTZEntry struct {
	beat   uint16
	tempo  uint16
	timeMS uint32
}

type indepPQT2 struct {
	flags    uint32
	first    indepPQTZEntry
	last     indepPQTZEntry
	count    uint32
	checksum uint32
	subUS    []uint16
}

type indepANLZ struct {
	pmaiHeader []byte
	sections   []indepANLZSection
	ppth       string
	pcobs      []indepPCOB
	pco2s      []indepPCO2
	pqtz       []indepPQTZEntry
	pqt2       *indepPQT2
}

func parseANLZIndependent(raw []byte) (*indepANLZ, error) {
	if len(raw) < 12 || string(raw[:4]) != "PMAI" {
		return nil, fmt.Errorf("missing PMAI magic")
	}
	hdrLen := int(binary.BigEndian.Uint32(raw[4:8]))
	fileLen := int(binary.BigEndian.Uint32(raw[8:12]))
	if hdrLen < 12 || hdrLen > fileLen || fileLen != len(raw) {
		return nil, fmt.Errorf("invalid PMAI bounds hdr=%d file=%d actual=%d", hdrLen, fileLen, len(raw))
	}
	out := &indepANLZ{pmaiHeader: append([]byte(nil), raw[:hdrLen]...)}
	for off := hdrLen; off < fileLen; {
		if fileLen-off < 12 {
			return nil, fmt.Errorf("truncated section at %d", off)
		}
		tag := string(raw[off : off+4])
		sh := int(binary.BigEndian.Uint32(raw[off+4 : off+8]))
		sn := int(binary.BigEndian.Uint32(raw[off+8 : off+12]))
		if sh < 12 || sn < sh || off+sn > fileLen {
			return nil, fmt.Errorf("bad %s bounds sh=%d sn=%d", tag, sh, sn)
		}
		sec := append([]byte(nil), raw[off:off+sn]...)
		isec := indepANLZSection{tag: tag, hdr: sh, raw: sec}
		switch tag {
		case "PPTH":
			if sn < 16 {
				return nil, fmt.Errorf("short PPTH")
			}
			plen := int(binary.BigEndian.Uint32(sec[12:16]))
			if 16+plen > sn || plen%2 != 0 {
				return nil, fmt.Errorf("invalid PPTH length %d", plen)
			}
			u := make([]uint16, plen/2)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(sec[16+2*i:])
			}
			out.ppth = strings.TrimRight(string(utf16.Decode(u)), "\x00")
		case "PCOB":
			if sh != 24 || sn < 24 {
				return nil, fmt.Errorf("bad PCOB header %d/%d", sh, sn)
			}
			kind := binary.BigEndian.Uint32(sec[12:16])
			isec.kind = kind
			if unk := binary.BigEndian.Uint16(sec[16:18]); unk != 0 {
				return nil, fmt.Errorf("PCOB nonzero unk 0x%04x", unk)
			}
			cnt := int(binary.BigEndian.Uint16(sec[18:20]))
			memCnt := binary.BigEndian.Uint32(sec[20:24])
			pcob := indepPCOB{kind: kind, memCount: memCnt}
			poff := 24
			for i := 0; i < cnt; i++ {
				if poff+56 > sn || string(sec[poff:poff+4]) != "PCPT" {
					return nil, fmt.Errorf("invalid PCPT[%d]", i)
				}
				ph := int(binary.BigEndian.Uint32(sec[poff+4 : poff+8]))
				pn := int(binary.BigEndian.Uint32(sec[poff+8 : poff+12]))
				if ph != 28 || pn != 56 || poff+pn > sn {
					return nil, fmt.Errorf("bad PCPT[%d] header/len %d/%d", i, ph, pn)
				}
				if sec[poff+30] != 0x03 || sec[poff+31] != 0xe8 {
					return nil, fmt.Errorf("PCPT[%d] bad u3 %x", i, sec[poff+30:poff+32])
				}
				pcob.entries = append(pcob.entries, indepPCPT{
					hotCue:     binary.BigEndian.Uint32(sec[poff+12 : poff+16]),
					status:     binary.BigEndian.Uint32(sec[poff+16 : poff+20]),
					u1:         binary.BigEndian.Uint32(sec[poff+20 : poff+24]),
					orderFirst: binary.BigEndian.Uint16(sec[poff+24 : poff+26]),
					orderLast:  binary.BigEndian.Uint16(sec[poff+26 : poff+28]),
					cueType:    sec[poff+28],
					timeMS:     binary.BigEndian.Uint32(sec[poff+32 : poff+36]),
					loopTimeMS: binary.BigEndian.Uint32(sec[poff+36 : poff+40]),
				})
				poff += pn
			}
			if poff != sn {
				return nil, fmt.Errorf("PCOB trailing bytes %d != %d", poff, sn)
			}
			out.pcobs = append(out.pcobs, pcob)
		case "PCO2":
			if sh != 20 || sn < 20 {
				return nil, fmt.Errorf("bad PCO2 header %d/%d", sh, sn)
			}
			kind := binary.BigEndian.Uint32(sec[12:16])
			isec.kind = kind
			cnt := int(binary.BigEndian.Uint16(sec[16:18]))
			pco2 := indepPCO2{kind: kind}
			poff := 20
			for i := 0; i < cnt; i++ {
				if poff+44 > sn || string(sec[poff:poff+4]) != "PCP2" {
					return nil, fmt.Errorf("invalid PCP2[%d]", i)
				}
				ph := int(binary.BigEndian.Uint32(sec[poff+4 : poff+8]))
				pn := int(binary.BigEndian.Uint32(sec[poff+8 : poff+12]))
				if ph != 16 || pn < 44 || poff+pn > sn {
					return nil, fmt.Errorf("bad PCP2[%d] header/len %d/%d", i, ph, pn)
				}
				if sec[poff+18] != 0x03 || sec[poff+19] != 0xe8 {
					return nil, fmt.Errorf("PCP2[%d] bad u2 %x", i, sec[poff+18:poff+20])
				}
				clen := int(binary.BigEndian.Uint32(sec[poff+40 : poff+44]))
				if clen%2 != 0 || 44+clen > pn {
					return nil, fmt.Errorf("PCP2[%d] bad comment len %d in %d", i, clen, pn)
				}
				u := make([]uint16, clen/2)
				for j := range u {
					u[j] = binary.BigEndian.Uint16(sec[poff+44+2*j:])
				}
				e := indepPCP2{
					lenEntry:   pn,
					hotCue:     binary.BigEndian.Uint32(sec[poff+12 : poff+16]),
					cueType:    sec[poff+16],
					timeMS:     binary.BigEndian.Uint32(sec[poff+20 : poff+24]),
					loopTimeMS: binary.BigEndian.Uint32(sec[poff+24 : poff+28]),
					colorID:    sec[poff+28],
					b29:        sec[poff+29],
					loopNum:    binary.BigEndian.Uint16(sec[poff+36 : poff+38]),
					loopDen:    binary.BigEndian.Uint16(sec[poff+38 : poff+40]),
					comment:    strings.TrimRight(string(utf16.Decode(u)), "\x00"),
				}
				q := poff + 44 + clen
				if poff+pn >= q+4 {
					e.hasTailRGB = true
					e.colorCode = sec[q]
					e.rgb = [3]byte{sec[q+1], sec[q+2], sec[q+3]}
				}
				pco2.entries = append(pco2.entries, e)
				poff += pn
			}
			if poff != sn {
				return nil, fmt.Errorf("PCO2 trailing bytes %d != %d", poff, sn)
			}
			out.pco2s = append(out.pco2s, pco2)
		case "PQTZ":
			if sh != 24 || sn < 24 {
				return nil, fmt.Errorf("bad PQTZ header %d/%d", sh, sn)
			}
			cnt := int(binary.BigEndian.Uint32(sec[20:24]))
			if 24+8*cnt != sn {
				return nil, fmt.Errorf("PQTZ count %d mismatches len %d", cnt, sn)
			}
			for i := 0; i < cnt; i++ {
				p := sec[24+8*i:]
				out.pqtz = append(out.pqtz, indepPQTZEntry{
					beat:   binary.BigEndian.Uint16(p[0:2]),
					tempo:  binary.BigEndian.Uint16(p[2:4]),
					timeMS: binary.BigEndian.Uint32(p[4:8]),
				})
			}
		case "PQT2":
			if sh != 56 || sn < 56 {
				return nil, fmt.Errorf("bad PQT2 header %d/%d", sh, sn)
			}
			cnt := binary.BigEndian.Uint32(sec[40:44])
			if 56+2*int(cnt) != sn {
				return nil, fmt.Errorf("PQT2 count %d mismatches len %d", cnt, sn)
			}
			q2 := &indepPQT2{
				flags: binary.BigEndian.Uint32(sec[16:20]),
				first: indepPQTZEntry{
					beat:   binary.BigEndian.Uint16(sec[24:26]),
					tempo:  binary.BigEndian.Uint16(sec[26:28]),
					timeMS: binary.BigEndian.Uint32(sec[28:32]),
				},
				last: indepPQTZEntry{
					beat:   binary.BigEndian.Uint16(sec[32:34]),
					tempo:  binary.BigEndian.Uint16(sec[34:36]),
					timeMS: binary.BigEndian.Uint32(sec[36:40]),
				},
				count:    cnt,
				checksum: binary.BigEndian.Uint32(sec[44:48]),
				subUS:    make([]uint16, int(cnt)),
			}
			for i := range q2.subUS {
				q2.subUS[i] = binary.BigEndian.Uint16(sec[56+2*i:])
			}
			out.pqt2 = q2
		}
		out.sections = append(out.sections, isec)
		off += sn
	}
	return out, nil
}

func assertNonMatchingSectionsIdentical(t *testing.T, label string, before, after *indepANLZ, rewrittenTags map[string]bool) {
	t.Helper()
	if !bytes.Equal(before.pmaiHeader[:8], after.pmaiHeader[:8]) || !bytes.Equal(before.pmaiHeader[12:], after.pmaiHeader[12:]) {
		t.Fatalf("%s: PMAI header non-length bytes changed", label)
	}
	if len(before.sections) != len(after.sections) {
		t.Fatalf("%s: section count changed %d -> %d", label, len(before.sections), len(after.sections))
	}
	for i := range before.sections {
		bs, as := before.sections[i], after.sections[i]
		if bs.tag != as.tag {
			t.Fatalf("%s: section[%d] tag changed %s -> %s", label, i, bs.tag, as.tag)
		}
		if rewrittenTags[bs.tag] {
			continue
		}
		if !bytes.Equal(bs.raw, as.raw) {
			t.Fatalf("%s: non-target section[%d] %s was modified (%d vs %d bytes)", label, i, bs.tag, len(bs.raw), len(as.raw))
		}
	}
}

func TestRewriteANLZCuesByteForByteFixtures(t *testing.T) {
	compactDAT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	compactEXT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}
	fullDAT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_mem_full.dat"))
	if err != nil {
		t.Fatal(err)
	}
	fullEXT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_mem_full.ext"))
	if err != nil {
		t.Fatal(err)
	}

	// Sanitized real rekordbox 7 fixtures must never trigger ANLZNeedsCueRepair.
	for name, tc := range map[string]struct {
		raw []byte
		ext bool
	}{
		"rb_hot_compact.dat":  {compactDAT, false},
		"rb_hot_compact.ext":  {compactEXT, true},
		"rb_hot_mem_full.dat": {fullDAT, false},
		"rb_hot_mem_full.ext": {fullEXT, true},
	} {
		if ANLZNeedsCueRepair(tc.raw, tc.ext) {
			t.Fatalf("real rekordbox fixture %s falsely triggered ANLZNeedsCueRepair", name)
		}
	}

	cDatBefore, err := parseANLZIndependent(compactDAT)
	if err != nil {
		t.Fatalf("parse compactDAT: %v", err)
	}
	cExtBefore, err := parseANLZIndependent(compactEXT)
	if err != nil {
		t.Fatalf("parse compactEXT: %v", err)
	}
	if cDatBefore.ppth != "/Contents/FixtureA/Track01.mp3" || cExtBefore.ppth != "/Contents/FixtureA/Track01.mp3" {
		t.Fatalf("unexpected fixture A PPTH: dat=%q ext=%q", cDatBefore.ppth, cExtBefore.ppth)
	}

	t.Run("CompactUnnamedUncoloredHotCueA_ExactBytes", func(t *testing.T) {
		cues := []Cue{{HotCue: 1, TimeMS: 12345}}
		datOut, changed, err := RewriteANLZCues(compactDAT, cues, false, false)
		if err != nil || !changed {
			t.Fatalf("RewriteANLZCues DAT: changed=%v err=%v", changed, err)
		}
		extOut, changed, err := RewriteANLZCues(compactEXT, cues, true, false)
		if err != nil || !changed {
			t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
		}

		cDatAfter, err := parseANLZIndependent(datOut)
		if err != nil {
			t.Fatalf("indep parse DAT: %v", err)
		}
		cExtAfter, err := parseANLZIndependent(extOut)
		if err != nil {
			t.Fatalf("indep parse EXT: %v", err)
		}
		assertNonMatchingSectionsIdentical(t, "compact DAT", cDatBefore, cDatAfter, map[string]bool{"PCOB": true})
		assertNonMatchingSectionsIdentical(t, "compact EXT", cExtBefore, cExtAfter, map[string]bool{"PCOB": true, "PCO2": true})

		// Verify exact section order: hot (1) before memory (0).
		if len(cDatAfter.pcobs) != 2 || cDatAfter.pcobs[0].kind != 1 || cDatAfter.pcobs[1].kind != 0 {
			t.Fatalf("DAT PCOB order wrong: %+v", cDatAfter.pcobs)
		}
		if len(cExtAfter.pcobs) != 2 || cExtAfter.pcobs[0].kind != 1 || cExtAfter.pcobs[1].kind != 0 ||
			len(cExtAfter.pcobs[0].entries) != 0 || len(cExtAfter.pcobs[1].entries) != 0 {
			t.Fatalf("EXT PCOB must be [1:0, 0:0] for hot A point cue, got %+v", cExtAfter.pcobs)
		}
		if len(cExtAfter.pco2s) != 2 || cExtAfter.pco2s[0].kind != 1 || cExtAfter.pco2s[1].kind != 0 {
			t.Fatalf("EXT PCO2 order wrong: %+v", cExtAfter.pco2s)
		}

		// Verify exact PCPT fields matching rekordbox 7 compact hot cue profile.
		if len(cDatAfter.pcobs[0].entries) != 1 {
			t.Fatalf("DAT PCOB(1) entries=%d, want 1", len(cDatAfter.pcobs[0].entries))
		}
		pt := cDatAfter.pcobs[0].entries[0]
		if pt.hotCue != 1 || pt.status != 1 || pt.u1 != 0 || pt.orderFirst != 0xffff || pt.orderLast != 0xffff || pt.cueType != 1 || pt.timeMS != 12345 || pt.loopTimeMS != 0 {
			t.Fatalf("unexpected compact PCPT: %+v", pt)
		}

		// Verify exact 44-byte PCP2 hex (b29=0x03, loop_time=0, len_comment=0).
		var pco2Raw []byte
		for _, s := range cExtAfter.sections {
			if s.tag == "PCO2" && s.kind == 1 {
				pco2Raw = s.raw[20:]
				break
			}
		}
		wantHex := "50435032000000100000002c00000001010003e8000030390000000000030000000000000000000000000000"
		if gotHex := hex.EncodeToString(pco2Raw); gotHex != wantHex {
			t.Fatalf("compact uncolored PCP2 bytes mismatch:\ngot:  %s\nwant: %s", gotHex, wantHex)
		}

		// Cross-check with ParseAnalysis.
		aDat, err := ParseAnalysis(datOut)
		if err != nil {
			t.Fatalf("ParseAnalysis DAT: %v", err)
		}
		aExt, err := ParseAnalysis(extOut)
		if err != nil {
			t.Fatalf("ParseAnalysis EXT: %v", err)
		}
		aDat.Merge(aExt)
		if len(aDat.Cues) != 1 || aDat.Cues[0].HotCue != 1 || aDat.Cues[0].TimeMS != 12345 || aDat.Cues[0].Name != "" || aDat.Cues[0].Color != ([4]byte{}) || aDat.Cues[0].ColorCode != 0 {
			t.Fatalf("ParseAnalysis round-trip mismatch: %+v", aDat.Cues)
		}
		if ANLZNeedsCueRepair(datOut, false) || ANLZNeedsCueRepair(extOut, true) {
			t.Fatal("rewritten compact files triggered ANLZNeedsCueRepair")
		}
	})

	t.Run("CompactUnnamedRGBColoredHotCueA_48Bytes", func(t *testing.T) {
		cues := []Cue{{HotCue: 1, TimeMS: 54321, Color: [4]byte{255, 0xf4, 0xd3, 0x38}}}
		extOut, changed, err := RewriteANLZCues(compactEXT, cues, true, false)
		if err != nil || !changed {
			t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
		}
		cExtAfter, err := parseANLZIndependent(extOut)
		if err != nil {
			t.Fatalf("indep parse EXT: %v", err)
		}
		assertNonMatchingSectionsIdentical(t, "compact colored EXT", cExtBefore, cExtAfter, map[string]bool{"PCOB": true, "PCO2": true})
		if len(cExtAfter.pco2s[0].entries) != 1 {
			t.Fatalf("want 1 PCP2 entry, got %d", len(cExtAfter.pco2s[0].entries))
		}
		e := cExtAfter.pco2s[0].entries[0]
		if e.lenEntry != 48 || e.b29 != 0x03 || e.comment != "" || !e.hasTailRGB || e.colorCode != 0 || e.rgb != ([3]byte{0xf4, 0xd3, 0x38}) {
			t.Fatalf("compact colored PCP2 mismatch: %+v", e)
		}
		aExt, err := ParseAnalysis(extOut)
		if err != nil {
			t.Fatalf("ParseAnalysis EXT: %v", err)
		}
		if len(aExt.Cues) != 1 || aExt.Cues[0].Color != ([4]byte{255, 0xf4, 0xd3, 0x38}) || aExt.Cues[0].ColorCode != 0 || aExt.Cues[0].Name != "" {
			t.Fatalf("ParseAnalysis colored compact mismatch: %+v", aExt.Cues)
		}
	})

	t.Run("FullHotAToHAndMemorySplit_FixtureB", func(t *testing.T) {
		fDatBefore, err := parseANLZIndependent(fullDAT)
		if err != nil {
			t.Fatalf("parse fullDAT: %v", err)
		}
		fExtBefore, err := parseANLZIndependent(fullEXT)
		if err != nil {
			t.Fatalf("parse fullEXT: %v", err)
		}
		if fDatBefore.ppth != "/Contents/FixtureB/Track02.mp3" || fExtBefore.ppth != "/Contents/FixtureB/Track02.mp3" {
			t.Fatalf("unexpected fixture B PPTH: dat=%q ext=%q", fDatBefore.ppth, fExtBefore.ppth)
		}

		cues := []Cue{
			{HotCue: 1, TimeMS: 1000, Name: "Intro", Color: [4]byte{255, 0x30, 0x5a, 0xff}, ColorCode: 1},
			{HotCue: 3, TimeMS: 3000},
			{HotCue: 4, TimeMS: 4000, Name: "Drop D", Color: [4]byte{255, 0x00, 0xe0, 0xff}, ColorCode: 9},
			{HotCue: 8, TimeMS: 10000, EndMS: 14000, Loop: true, Color: [4]byte{255, 0xaa, 0xbb, 0xcc}},
			{HotCue: 0, TimeMS: 500, Name: "Mem1", ColorID: 2},
			{HotCue: 0, TimeMS: 2500, ColorID: 5},
			{HotCue: 0, TimeMS: 6000, EndMS: 8000, Loop: true, ColorID: 0},
		}
		datOut, changed, err := RewriteANLZCues(fullDAT, cues, false, false)
		if err != nil || !changed {
			t.Fatalf("RewriteANLZCues DAT: changed=%v err=%v", changed, err)
		}
		extOut, changed, err := RewriteANLZCues(fullEXT, cues, true, false)
		if err != nil || !changed {
			t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
		}
		fDatAfter, err := parseANLZIndependent(datOut)
		if err != nil {
			t.Fatalf("indep parse DAT: %v", err)
		}
		fExtAfter, err := parseANLZIndependent(extOut)
		if err != nil {
			t.Fatalf("indep parse EXT: %v", err)
		}
		assertNonMatchingSectionsIdentical(t, "full DAT", fDatBefore, fDatAfter, map[string]bool{"PCOB": true})
		assertNonMatchingSectionsIdentical(t, "full EXT", fExtBefore, fExtAfter, map[string]bool{"PCOB": true, "PCO2": true})

		// .DAT PCOB(1) holds ONLY Hot A..C (1, 3); .DAT PCOB(0) holds 3 memory cues with memCount=2.
		if len(fDatAfter.pcobs[0].entries) != 2 || fDatAfter.pcobs[0].entries[0].hotCue != 1 || fDatAfter.pcobs[0].entries[1].hotCue != 3 {
			t.Fatalf("DAT PCOB(1) should hold only A..C (1,3), got %+v", fDatAfter.pcobs[0].entries)
		}
		if len(fDatAfter.pcobs[1].entries) != 3 || fDatAfter.pcobs[1].memCount != 2 {
			t.Fatalf("DAT PCOB(0) want 3 memory cues with memCount=2, got count=%d memCount=%d", len(fDatAfter.pcobs[1].entries), fDatAfter.pcobs[1].memCount)
		}
		m := fDatAfter.pcobs[1].entries
		if m[0].orderFirst != 0xffff || m[0].orderLast != 1 || m[1].orderFirst != 0 || m[1].orderLast != 2 || m[2].orderFirst != 1 || m[2].orderLast != 0xffff {
			t.Fatalf("DAT PCOB(0) memory linked list wrong: %+v", m)
		}
		// .EXT PCOB(1) holds ONLY Hot D..H (4, 8); .EXT PCOB(0) is empty.
		if len(fExtAfter.pcobs[0].entries) != 2 || fExtAfter.pcobs[0].entries[0].hotCue != 4 || fExtAfter.pcobs[0].entries[1].hotCue != 8 {
			t.Fatalf("EXT PCOB(1) should hold only D..H (4,8), got %+v", fExtAfter.pcobs[0].entries)
		}
		if len(fExtAfter.pcobs[1].entries) != 0 {
			t.Fatalf("EXT PCOB(0) should be empty, got %+v", fExtAfter.pcobs[1].entries)
		}
		// .EXT PCO2(1) has all 4 hot cues with exact profiles; PCO2(0) has all 3 memory cues.
		if len(fExtAfter.pco2s[0].entries) != 4 || len(fExtAfter.pco2s[1].entries) != 3 {
			t.Fatalf("EXT PCO2 counts=%d/%d, want 4/3", len(fExtAfter.pco2s[0].entries), len(fExtAfter.pco2s[1].entries))
		}
		h0, h1, h2, h3 := fExtAfter.pco2s[0].entries[0], fExtAfter.pco2s[0].entries[1], fExtAfter.pco2s[0].entries[2], fExtAfter.pco2s[0].entries[3]
		if h0.b29 != 0x01 || h0.comment != "Intro" || h0.colorCode != 1 {
			t.Fatalf("hot A mismatch: %+v", h0)
		}
		if h1.b29 != 0x03 || h1.lenEntry != 44 {
			t.Fatalf("hot C compact mismatch: %+v", h1)
		}
		if h2.b29 != 0x01 || h2.comment != "Drop D" || h2.colorCode != 9 {
			t.Fatalf("hot D mismatch: %+v", h2)
		}
		if h3.b29 != 0x03 || h3.lenEntry != 48 || h3.cueType != 2 || h3.loopTimeMS != 14000 || h3.rgb != ([3]byte{0xaa, 0xbb, 0xcc}) {
			t.Fatalf("hot H compact loop mismatch: %+v", h3)
		}
		for i, me := range fExtAfter.pco2s[1].entries {
			if me.b29 != 0x01 || me.colorCode != 0 || me.rgb != ([3]byte{}) {
				t.Fatalf("memory[%d] mismatch: %+v", i, me)
			}
		}
		aDat, err := ParseAnalysis(datOut)
		if err != nil {
			t.Fatal(err)
		}
		aExt, err := ParseAnalysis(extOut)
		if err != nil {
			t.Fatal(err)
		}
		aDat.Merge(aExt)
		if len(aDat.Cues) != 7 {
			t.Fatalf("merged ParseAnalysis got %d cues, want 7: %+v", len(aDat.Cues), aDat.Cues)
		}
		if ANLZNeedsCueRepair(datOut, false) || ANLZNeedsCueRepair(extOut, true) {
			t.Fatal("rewritten full files triggered ANLZNeedsCueRepair")
		}
	})
}

func TestANLZNeedsCueRepairAndRoundTrip(t *testing.T) {
	compactDAT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	compactEXT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}

	// Build swapped/legacy EngineX DAT: PCOB(0) before PCOB(1) with memory_count == count.
	makeSwappedDAT := func(raw []byte) []byte {
		h, secs, err := parseANLZSections(raw)
		if err != nil {
			t.Fatal(err)
		}
		pcob0 := buildPCOBSection(0, []Cue{{HotCue: 0, TimeMS: 500}})
		binary.BigEndian.PutUint32(pcob0[20:], 1) // buggy count instead of count-1
		pcob1 := buildPCOBSection(1, []Cue{{HotCue: 1, TimeMS: 1200}})
		var out [][]byte
		for _, s := range secs {
			if s.tag == "PCOB" {
				continue
			}
			out = append(out, s.raw)
		}
		out = append(out, pcob0, pcob1)
		return assembleANLZ(raw[:h], out)
	}

	// Build swapped/legacy EngineX EXT: PCOB(0) before PCOB(1) with A..C cue, PCO2(0) before PCO2(1)
	// with legacy b29=0 truncated 60-byte PCP2.
	makeSwappedEXT := func(raw []byte) []byte {
		h, secs, err := parseANLZSections(raw)
		if err != nil {
			t.Fatal(err)
		}
		pcob0 := buildPCOBSection(0, []Cue{{HotCue: 0, TimeMS: 500}})
		pcob1 := buildPCOBSection(1, []Cue{{HotCue: 1, TimeMS: 1200}})
		pco20 := buildPCO2Section(0, []Cue{{HotCue: 0, TimeMS: 500}})
		// Legacy 60-byte b29=0 PCP2 inside PCO2(1)
		pcp2 := make([]byte, 60)
		copy(pcp2, "PCP2")
		binary.BigEndian.PutUint32(pcp2[4:], 16)
		binary.BigEndian.PutUint32(pcp2[8:], 60)
		binary.BigEndian.PutUint32(pcp2[12:], 1)
		pcp2[16] = 1
		pcp2[18], pcp2[19] = 3, 0xe8
		binary.BigEndian.PutUint32(pcp2[20:], 1200)
		binary.BigEndian.PutUint32(pcp2[24:], 0xffffffff)
		pcp2[29] = 0x00 // legacy buggy byte 29
		pco21 := make([]byte, 20+len(pcp2))
		copy(pco21, "PCO2")
		binary.BigEndian.PutUint32(pco21[4:], 20)
		binary.BigEndian.PutUint32(pco21[8:], uint32(len(pco21)))
		binary.BigEndian.PutUint32(pco21[12:], 1)
		binary.BigEndian.PutUint16(pco21[16:], 1)
		copy(pco21[20:], pcp2)

		var out [][]byte
		inserted := false
		for _, s := range secs {
			if s.tag == "PCOB" || s.tag == "PCO2" {
				if !inserted {
					out = append(out, pcob0, pcob1, pco20, pco21)
					inserted = true
				}
				continue
			}
			out = append(out, s.raw)
		}
		return assembleANLZ(raw[:h], out)
	}

	swappedDAT := makeSwappedDAT(compactDAT)
	swappedEXT := makeSwappedEXT(compactEXT)
	if !ANLZNeedsCueRepair(swappedDAT, false) {
		t.Fatal("swapped DAT failed to trigger ANLZNeedsCueRepair")
	}
	if !ANLZNeedsCueRepair(swappedEXT, true) {
		t.Fatal("swapped EXT failed to trigger ANLZNeedsCueRepair")
	}

	cDatBefore, err := parseANLZIndependent(swappedDAT)
	if err != nil {
		t.Fatal(err)
	}
	cExtBefore, err := parseANLZIndependent(swappedEXT)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAnalysis(swappedDAT)
	if err != nil {
		t.Fatal(err)
	}
	parsedExt, err := ParseAnalysis(swappedEXT)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Merge(parsedExt)

	repairedDAT, changed, err := RewriteANLZCues(swappedDAT, parsed.Cues, false, false)
	if err != nil || !changed {
		t.Fatalf("repair DAT: changed=%v err=%v", changed, err)
	}
	repairedEXT, changed, err := RewriteANLZCues(swappedEXT, parsed.Cues, true, false)
	if err != nil || !changed {
		t.Fatalf("repair EXT: changed=%v err=%v", changed, err)
	}
	if ANLZNeedsCueRepair(repairedDAT, false) {
		t.Fatal("repaired DAT still triggers ANLZNeedsCueRepair")
	}
	if ANLZNeedsCueRepair(repairedEXT, true) {
		t.Fatal("repaired EXT still triggers ANLZNeedsCueRepair")
	}
	cDatAfter, err := parseANLZIndependent(repairedDAT)
	if err != nil {
		t.Fatal(err)
	}
	cExtAfter, err := parseANLZIndependent(repairedEXT)
	if err != nil {
		t.Fatal(err)
	}
	assertNonMatchingSectionsIdentical(t, "repaired DAT", cDatBefore, cDatAfter, map[string]bool{"PCOB": true})
	assertNonMatchingSectionsIdentical(t, "repaired EXT", cExtBefore, cExtAfter, map[string]bool{"PCOB": true, "PCO2": true})
	if cDatAfter.pcobs[0].kind != 1 || cDatAfter.pcobs[1].kind != 0 ||
		cExtAfter.pcobs[0].kind != 1 || cExtAfter.pcobs[1].kind != 0 ||
		cExtAfter.pco2s[0].kind != 1 || cExtAfter.pco2s[1].kind != 0 {
		t.Fatal("repaired files did not restore hot-before-memory order")
	}
}

func TestRewriteANLZGridRoundTripAndByteIdentity(t *testing.T) {
	for _, fx := range []struct {
		name, datPath, extPath string
	}{
		{"compact", filepath.Join("testdata", "rb_hot_compact.dat"), filepath.Join("testdata", "rb_hot_compact.ext")},
		{"full", filepath.Join("testdata", "rb_hot_mem_full.dat"), filepath.Join("testdata", "rb_hot_mem_full.ext")},
	} {
		t.Run(fx.name, func(t *testing.T) {
			rawDAT, err := os.ReadFile(fx.datPath)
			if err != nil {
				t.Fatal(err)
			}
			rawEXT, err := os.ReadFile(fx.extPath)
			if err != nil {
				t.Fatal(err)
			}
			datBefore, err := parseANLZIndependent(rawDAT)
			if err != nil {
				t.Fatalf("indep parse orig DAT: %v", err)
			}
			extBefore, err := parseANLZIndependent(rawEXT)
			if err != nil {
				t.Fatalf("indep parse orig EXT: %v", err)
			}
			// Verify real rekordbox fixture's PQT2 checksum matches sum(beat+tempo+timeMS).
			if extBefore.pqt2 == nil || int(extBefore.pqt2.count) != len(datBefore.pqtz) {
				t.Fatalf("fixture %s missing matching PQT2/PQTZ (%v vs %d)", fx.name, extBefore.pqt2, len(datBefore.pqtz))
			}
			var origSum uint64
			for _, b := range datBefore.pqtz {
				origSum += uint64(b.beat) + uint64(b.tempo) + uint64(b.timeMS)
			}
			if uint32(origSum) != extBefore.pqt2.checksum {
				t.Fatalf("fixture %s PQT2 checksum=0x%08x, formula=0x%08x", fx.name, extBefore.pqt2.checksum, uint32(origSum))
			}

			// Constant-BPM grid with explicit sub-millisecond offsets.
			constBeats := []Beat{
				{Number: 1, BPM: 125.00, TimeMS: 120, SubUS: 125},
				{Number: 2, BPM: 125.00, TimeMS: 600, SubUS: 125},
				{Number: 3, BPM: 125.00, TimeMS: 1080, SubUS: 125},
				{Number: 4, BPM: 125.00, TimeMS: 1560, SubUS: 125},
				{Number: 1, BPM: 125.00, TimeMS: 2040, SubUS: 125},
			}
			datOut, changed, err := RewriteANLZGrid(rawDAT, constBeats, false)
			if err != nil || !changed {
				t.Fatalf("RewriteANLZGrid DAT: changed=%v err=%v", changed, err)
			}
			extOut, changed, err := RewriteANLZGrid(rawEXT, constBeats, true)
			if err != nil || !changed {
				t.Fatalf("RewriteANLZGrid EXT: changed=%v err=%v", changed, err)
			}
			datAfter, err := parseANLZIndependent(datOut)
			if err != nil {
				t.Fatalf("indep parse rewritten DAT: %v", err)
			}
			extAfter, err := parseANLZIndependent(extOut)
			if err != nil {
				t.Fatalf("indep parse rewritten EXT: %v", err)
			}
			assertNonMatchingSectionsIdentical(t, fx.name+" grid DAT", datBefore, datAfter, map[string]bool{"PQTZ": true})
			assertNonMatchingSectionsIdentical(t, fx.name+" grid EXT", extBefore, extAfter, map[string]bool{"PQT2": true})

			if extAfter.pqt2 == nil || extAfter.pqt2.flags != 0x01000002 || extAfter.pqt2.count != uint32(len(constBeats)) {
				t.Fatalf("const PQT2 header wrong: %+v", extAfter.pqt2)
			}
			if extAfter.pqt2.first != datAfter.pqtz[0] || extAfter.pqt2.last != datAfter.pqtz[len(datAfter.pqtz)-1] {
				t.Fatalf("const PQT2 first/last mismatch: %+v vs %+v/%+v", extAfter.pqt2, datAfter.pqtz[0], datAfter.pqtz[len(datAfter.pqtz)-1])
			}
			var wantSum uint64
			for _, b := range datAfter.pqtz {
				wantSum += uint64(b.beat) + uint64(b.tempo) + uint64(b.timeMS)
			}
			if extAfter.pqt2.checksum != uint32(wantSum) {
				t.Fatalf("const PQT2 checksum=0x%08x want 0x%08x", extAfter.pqt2.checksum, uint32(wantSum))
			}

			// Verify ParseAnalysis merges PQTZ + PQT2 sub-ms cleanly.
			aDat, err := ParseAnalysis(datOut)
			if err != nil {
				t.Fatal(err)
			}
			aExt, err := ParseAnalysis(extOut)
			if err != nil {
				t.Fatal(err)
			}
			aDat.Merge(aExt)
			if len(aDat.Beats) != len(constBeats) {
				t.Fatalf("merged beats=%d want %d", len(aDat.Beats), len(constBeats))
			}
			for i, b := range constBeats {
				got := aDat.Beats[i]
				if got.Number != b.Number || math.Abs(got.BPM-b.BPM) > 0.005 || got.TimeMS != b.TimeMS || got.SubUS != b.SubUS {
					t.Fatalf("beat[%d] got=%+v want=%+v", i, got, b)
				}
			}

			// Variable-BPM grid must set PQT2 flags = 0x02000002.
			varBeats := []Beat{
				{Number: 1, BPM: 124.00, TimeMS: 100, SubUS: 250},
				{Number: 2, BPM: 124.00, TimeMS: 583, SubUS: 871},
				{Number: 3, BPM: 128.50, TimeMS: 1067, SubUS: 742},
				{Number: 4, BPM: 128.50, TimeMS: 1534, SubUS: 668},
			}
			extVar, changed, err := RewriteANLZGrid(rawEXT, varBeats, true)
			if err != nil || !changed {
				t.Fatalf("RewriteANLZGrid variable EXT: changed=%v err=%v", changed, err)
			}
			extVarParsed, err := parseANLZIndependent(extVar)
			if err != nil {
				t.Fatal(err)
			}
			if extVarParsed.pqt2 == nil || extVarParsed.pqt2.flags != 0x02000002 {
				t.Fatalf("variable BPM PQT2 flags=0x%08x, want 0x02000002", extVarParsed.pqt2.flags)
			}
			for i, b := range varBeats {
				if extVarParsed.pqt2.subUS[i] != b.SubUS {
					t.Fatalf("var PQT2 subUS[%d]=%d want %d", i, extVarParsed.pqt2.subUS[i], b.SubUS)
				}
			}
		})
	}

	// .EXT without PQT2 must return changed=false untouched.
	noPQT2 := testfixture.EXT()
	out, changed, err := RewriteANLZGrid(noPQT2, []Beat{{Number: 1, BPM: 120, TimeMS: 0}}, true)
	if err != nil || changed || !bytes.Equal(out, noPQT2) {
		t.Fatalf("EXT without PQT2 should stay unchanged: changed=%v err=%v", changed, err)
	}
}

func TestUpdateOneLibraryAllKeysGridTracking(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "exportLibrary.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dbPath}).String())
	if err != nil {
		t.Fatal(err)
	}
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
			cueUpdateCount integer,
			analysisDataUpdateCount integer
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
		INSERT INTO content(content_id, title, bpmx100, length, path, samplingRate, djComment, analysisDataFilePath, hasModified, cueUpdateCount, analysisDataUpdateCount)
		VALUES (1, 'Track 1', 12800, 180, '/Contents/t1.mp3', 44100, '', '/PIONEER/USBANLZ/P001/ANLZ0000.DAT', 0, NULL, NULL);
	`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Pass 1: grid-only update.
	out1 := filepath.Join(dir, "out1.db")
	tm1, err := UpdateOneLibraryAllKeys(context.Background(), dbPath, nil, nil, map[uint32]GridUpdate{1: {BPM: 126.25}}, out1, nil)
	if err != nil {
		t.Fatalf("UpdateOneLibraryAllKeys grid-only: %v", err)
	}
	if tm1.WrittenBPM[1] != 126.25 {
		t.Fatalf("WrittenBPM[1]=%v, want 126.25", tm1.WrittenBPM[1])
	}

	// Pass 2: combined cue + grid update.
	out2 := filepath.Join(dir, "out2.db")
	tm2, err := UpdateOneLibraryAllKeys(context.Background(), out1,
		map[uint32][]Cue{1: {{HotCue: 1, TimeMS: 1200}}},
		nil,
		map[uint32]GridUpdate{1: {BPM: 128.00}},
		out2, nil)
	if err != nil {
		t.Fatalf("UpdateOneLibraryAllKeys combined: %v", err)
	}
	if tm2.WrittenBPM[1] != 128.00 {
		t.Fatalf("WrittenBPM[1]=%v, want 128.00", tm2.WrittenBPM[1])
	}

	checkDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: out2}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer checkDB.Close()
	var hasMod, bpm100 int
	var cueUpd, anlzUpd sql.NullInt64
	if err := checkDB.QueryRow(`SELECT hasModified, bpmx100, cueUpdateCount, analysisDataUpdateCount FROM content WHERE content_id=1`).Scan(&hasMod, &bpm100, &cueUpd, &anlzUpd); err != nil {
		t.Fatal(err)
	}
	if hasMod != 1 || bpm100 != 12800 || !cueUpd.Valid || cueUpd.Int64 != 1 || !anlzUpd.Valid || anlzUpd.Int64 != 2 {
		t.Fatalf("got hasMod=%d bpm100=%d cueUpd=%v anlzUpd=%v, want 1, 12800, 1, 2", hasMod, bpm100, cueUpd, anlzUpd)
	}
}

// TestRewriteANLZCuesMatchesRekordboxOracleByteForByte verifies that RewriteANLZCues produces
// byte-identical .DAT and .EXT files to the rekordbox compact hot-cue oracle (for one uncolored,
// unnamed hot cue A at T = 28264 ms).
func TestRewriteANLZCuesMatchesRekordboxOracleByteForByte(t *testing.T) {
	const cueMS = uint32(28264)

	origCompactDAT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	origCompactEXT, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.ext"))
	if err != nil {
		t.Fatal(err)
	}

	splitRawSecs := func(d []byte) (head []byte, secs []struct {
		tag string
		raw []byte
	}) {
		hl := int(binary.BigEndian.Uint32(d[4:8]))
		head = append([]byte(nil), d[:hl]...)
		for p := hl; p+12 <= len(d); {
			tLen := int(binary.BigEndian.Uint32(d[p+8 : p+12]))
			secs = append(secs, struct {
				tag string
				raw []byte
			}{tag: string(d[p : p+4]), raw: append([]byte(nil), d[p:p+tLen]...)})
			p += tLen
		}
		return head, secs
	}
	packRawSecs := func(head []byte, secs []struct {
		tag string
		raw []byte
	}) []byte {
		var body bytes.Buffer
		for _, s := range secs {
			body.Write(s.raw)
		}
		total := len(head) + body.Len()
		out := make([]byte, 0, total)
		out = append(out, head[:8]...)
		var totalBE [4]byte
		binary.BigEndian.PutUint32(totalBE[:], uint32(total))
		out = append(out, totalBE[:]...)
		out = append(out, head[12:]...)
		out = append(out, body.Bytes()...)
		return out
	}

	// Build empty-cue rekordbox-made originals (preserving all non-cue bytes verbatim) matching the
	// oracle script's precondition (empty PCOB(1) first in .DAT; empty PCO2(1) first in .EXT).
	makeEmptyDAT := func(d []byte) []byte {
		head, secs := splitRawSecs(d)
		for i := range secs {
			if secs[i].tag == "PCOB" && binary.BigEndian.Uint32(secs[i].raw[12:16]) == 1 {
				empty := append([]byte(nil), secs[i].raw[:24]...)
				binary.BigEndian.PutUint32(empty[8:12], 24)
				binary.BigEndian.PutUint16(empty[18:20], 0)
				secs[i].raw = empty
			}
		}
		return packRawSecs(head, secs)
	}
	makeEmptyEXT := func(d []byte) []byte {
		head, secs := splitRawSecs(d)
		for i := range secs {
			if secs[i].tag == "PCO2" && binary.BigEndian.Uint32(secs[i].raw[12:16]) == 1 {
				empty := append([]byte(nil), secs[i].raw[:20]...)
				binary.BigEndian.PutUint32(empty[8:12], 20)
				binary.BigEndian.PutUint16(empty[16:18], 0)
				secs[i].raw = empty
			}
		}
		return packRawSecs(head, secs)
	}

	emptyDAT := makeEmptyDAT(origCompactDAT)
	emptyEXT := makeEmptyEXT(origCompactEXT)

	// Verbatim rekordbox compact hot-cue oracle bytes for T = 28264 ms:
	pcptPrefix, _ := hex.DecodeString("504350540000001c00000038000000010000000100000000ffffffff010003e8")
	var tBE [4]byte
	binary.BigEndian.PutUint32(tBE[:], cueMS)
	oraclePCPT := make([]byte, 0, 56)
	oraclePCPT = append(oraclePCPT, pcptPrefix...)
	oraclePCPT = append(oraclePCPT, tBE[:]...)
	oraclePCPT = append(oraclePCPT, make([]byte, 20)...)

	pcp2Prefix, _ := hex.DecodeString("50435032000000100000002c00000001010003e8")
	pcp2Suffix, _ := hex.DecodeString("0000000000030000000000000000000000000000")
	oraclePCP2 := make([]byte, 0, 44)
	oraclePCP2 = append(oraclePCP2, pcp2Prefix...)
	oraclePCP2 = append(oraclePCP2, tBE[:]...)
	oraclePCP2 = append(oraclePCP2, pcp2Suffix...)
	if len(oraclePCPT) != 56 || len(oraclePCP2) != 44 {
		t.Fatalf("bad oracle lengths: PCPT=%d PCP2=%d", len(oraclePCPT), len(oraclePCP2))
	}

	buildOracle := func(orig []byte, isExt bool) []byte {
		head, secs := splitRawSecs(orig)
		doneCOB, doneCO2 := false, false
		for i := range secs {
			s := secs[i].raw
			if secs[i].tag == "PCOB" && !isExt && !doneCOB {
				if binary.BigEndian.Uint32(s[12:16]) != 1 || s[18] != 0 || s[19] != 0 {
					t.Fatalf("first PCOB must be empty hot list")
				}
				h := append([]byte(nil), s[:24]...)
				binary.BigEndian.PutUint32(h[8:12], 24+56)
				binary.BigEndian.PutUint16(h[18:20], 1)
				secs[i].raw = append(h, oraclePCPT...)
				doneCOB = true
			} else if secs[i].tag == "PCO2" && !doneCO2 {
				if binary.BigEndian.Uint32(s[12:16]) != 1 || s[16] != 0 || s[17] != 0 {
					t.Fatalf("first PCO2 must be empty hot list")
				}
				h := append([]byte(nil), s[:20]...)
				binary.BigEndian.PutUint32(h[8:12], 20+44)
				binary.BigEndian.PutUint16(h[16:18], 1)
				secs[i].raw = append(h, oraclePCP2...)
				doneCO2 = true
			}
		}
		if (!isExt && !doneCOB) || (isExt && !doneCO2) {
			t.Fatalf("oracle section missing")
		}
		return packRawSecs(head, secs)
	}

	oracleDAT := buildOracle(emptyDAT, false)
	oracleEXT := buildOracle(emptyEXT, true)

	for _, tc := range []struct {
		name string
		dat  []byte
		ext  []byte
	}{
		{"FromEmptyOriginals", emptyDAT, emptyEXT},
		{"FromExistingCompactHot", origCompactDAT, origCompactEXT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cues := []Cue{{HotCue: 1, TimeMS: cueMS}}
			gotDAT, changed, err := RewriteANLZCues(tc.dat, cues, false, false)
			if err != nil || !changed {
				t.Fatalf("RewriteANLZCues DAT: changed=%v err=%v", changed, err)
			}
			gotEXT, changed, err := RewriteANLZCues(tc.ext, cues, true, false)
			if err != nil || !changed {
				t.Fatalf("RewriteANLZCues EXT: changed=%v err=%v", changed, err)
			}
			if !bytes.Equal(gotDAT, oracleDAT) {
				t.Fatalf("DAT differs from rekordbox compact oracle: got %d bytes, want %d bytes", len(gotDAT), len(oracleDAT))
			}
			if !bytes.Equal(gotEXT, oracleEXT) {
				t.Fatalf("EXT differs from rekordbox compact oracle: got %d bytes, want %d bytes", len(gotEXT), len(oracleEXT))
			}
		})
	}
}

func TestParseANLZPath(t *testing.T) {
	dat, err := os.ReadFile(filepath.Join("testdata", "rb_hot_compact.dat"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ParseANLZPath(dat)
	if !ok || got != "/Contents/FixtureA/Track01.mp3" {
		t.Fatalf("ParseANLZPath = (%q, %v), want (/Contents/FixtureA/Track01.mp3, true)", got, ok)
	}
	if _, ok := ParseANLZPath([]byte("bad")); ok {
		t.Fatal("ParseANLZPath succeeded on invalid bytes")
	}
}

func TestReviewOracleCueRewriteNoop(t *testing.T) {
	for _, stem := range []string{"rb_hot_compact", "rb_hot_mem_full"} {
		t.Run(stem, func(t *testing.T) {
			dat, err := os.ReadFile(filepath.Join("testdata", stem+".dat"))
			if err != nil {
				t.Fatal(err)
			}
			ext, err := os.ReadFile(filepath.Join("testdata", stem+".ext"))
			if err != nil {
				t.Fatal(err)
			}
			a, err := ParseAnalysis(dat)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseAnalysis(ext)
			if err != nil {
				t.Fatal(err)
			}
			a.Merge(b)
			for _, f := range []struct {
				name string
				raw  []byte
				ext  bool
			}{{"DAT", dat, false}, {"EXT", ext, true}} {
				got, changed, err := RewriteANLZCues(f.raw, a.Cues, f.ext, false)
				if err != nil {
					t.Fatal(err)
				}
				if changed || !bytes.Equal(got, f.raw) {
					first := 0
					for first < min(len(f.raw), len(got)) && f.raw[first] == got[first] {
						first++
					}
					t.Fatalf("%s unchanged cues altered file: changed=%v %d -> %d bytes; first difference at %d", f.name, changed, len(f.raw), len(got), first)
				}
			}
		})
	}
}

func TestRewriteANLZCuesPreservesUnchangedEntriesOnEdit(t *testing.T) {
	ext, err := os.ReadFile(filepath.Join("testdata", "rb_hot_mem_full.ext"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseAnalysis(ext)
	if err != nil {
		t.Fatal(err)
	}
	edited := append([]Cue(nil), a.Cues...)
	moved := false
	for i := range edited {
		if edited[i].HotCue == 1 {
			edited[i].TimeMS += 500
			moved = true
			break
		}
	}
	if !moved {
		t.Fatal("fixture missing HotCue 1")
	}
	got, changed, err := RewriteANLZCues(ext, edited, true, false)
	if err != nil || !changed {
		t.Fatalf("RewriteANLZCues edited: changed=%v err=%v", changed, err)
	}
	_, origSecs, err := parseANLZSections(ext)
	if err != nil {
		t.Fatal(err)
	}
	_, gotSecs, err := parseANLZSections(got)
	if err != nil {
		t.Fatal(err)
	}
	secByKey := func(secs []anlzSection, tag string, kind uint32) anlzSection {
		for _, s := range secs {
			if s.tag == tag && s.kind == kind {
				return s
			}
		}
		t.Fatalf("missing section %s:%d", tag, kind)
		return anlzSection{}
	}
	// PCOB(1) (cues 4..8), PCOB(0) (empty), and PCO2(0) (memory cues) are unchanged -> section-identical.
	for _, k := range []struct {
		tag  string
		kind uint32
	}{{"PCOB", 1}, {"PCOB", 0}, {"PCO2", 0}} {
		os := secByKey(origSecs, k.tag, k.kind)
		gs := secByKey(gotSecs, k.tag, k.kind)
		if !bytes.Equal(os.raw, gs.raw) {
			t.Fatalf("%s:%d changed on unrelated HotCue 1 edit", k.tag, k.kind)
		}
	}
	// Inside rebuilt PCO2(1), untouched entries (e.g. HotCue 2) keep exact raw bytes.
	origHot, ok1 := parseRawCueEntries(secByKey(origSecs, "PCO2", 1))
	gotHot, ok2 := parseRawCueEntries(secByKey(gotSecs, "PCO2", 1))
	if !ok1 || !ok2 {
		t.Fatal("parseRawCueEntries failed on PCO2:1")
	}
	var origHC2, gotHC2 []byte
	for _, e := range origHot {
		if e.cue.HotCue == 2 {
			origHC2 = e.raw
		}
	}
	for _, e := range gotHot {
		if e.cue.HotCue == 2 {
			gotHC2 = e.raw
		}
	}
	if len(origHC2) == 0 || !bytes.Equal(origHC2, gotHC2) {
		t.Fatalf("untouched HotCue 2 PCP2 entry changed: orig=%d got=%d", len(origHC2), len(gotHC2))
	}
}
