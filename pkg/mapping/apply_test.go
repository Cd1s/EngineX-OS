package mapping

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func applyDir(t *testing.T, mappings string) (dir, in, out, file, status string) {
	t.Helper()
	dir = t.TempDir()
	in = filepath.Join(dir, "stock.qml")
	out = filepath.Join(dir, "patched", "JP07_Controller_Assignments.qml")
	file = filepath.Join(dir, "mappings.json")
	status = StatusPath(filepath.Join(dir, "patched"), "JP07")
	if err := os.WriteFile(in, []byte(fixtureStock), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if mappings != "" {
		if err := os.WriteFile(file, []byte(mappings), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return
}

const oneMapping = `[{"board":"JP07","control":"PadModeSelect:28","gesture":"shift","action":"waveform_cycle"}]`

func TestApplyWritesThePatchedFileAndAStatus(t *testing.T) {
	_, in, out, file, status := applyDir(t, oneMapping)
	ok, err := Apply("JP07", in, out, file, status)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), `objectName: "EngineX Map PadModeSelect:28 shift"`) {
		t.Fatalf("%v\n%s", err, b)
	}
	st, err := ReadStatus(status)
	if err != nil || st.State != "applied" || st.Mappings != 1 || st.Board != "JP07" || st.Time == "" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestApplyWithNoMappingsBindsNothing(t *testing.T) {
	for name, content := range map[string]string{"no file": "", "an empty list": "[]", "another board": strings.ReplaceAll(oneMapping, "JP07", "JP13")} {
		_, in, out, file, status := applyDir(t, content)
		ok, err := Apply("JP07", in, out, file, status)
		if err != nil || ok {
			t.Errorf("%s: %v %v", name, ok, err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: a file was written", name)
		}
		if st, _ := ReadStatus(status); st.State != "none" {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

func TestApplyRejectsWhatFailsAndLeavesStockAlone(t *testing.T) {
	for name, c := range map[string]struct{ mappings, code string }{
		"an unknown action":   {strings.Replace(oneMapping, "waveform_cycle", "format_disk", 1), ReasonAction},
		"a reserved control":  {strings.Replace(oneMapping, "PadModeSelect:28", "Shift:26", 1), ReasonReserved},
		"a file that is junk": {"not json", "json"},
		"Shift + LOOP":        {strings.Replace(oneMapping, "PadModeSelect:28", "PadModeSelect:30", 1), ReasonTaken},
	} {
		_, in, out, file, status := applyDir(t, c.mappings)
		ok, err := Apply("JP07", in, out, file, status)
		if err == nil || ok {
			t.Errorf("%s: %v %v", name, ok, err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: a file was written", name)
		}
		st, _ := ReadStatus(status)
		if st.State != "rejected" || st.Code != c.code || st.Reason == "" {
			t.Errorf("%s: %+v", name, st)
		}
	}

}

func TestApplyRejectsAValidMappingWhenTheStockFileCannotGenerate(t *testing.T) {
	_, in, out, file, status := applyDir(t, oneMapping)
	if err := os.WriteFile(in, []byte("Item {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := Apply("JP07", in, out, file, status); ok || err == nil || !strings.Contains(err.Error(), "not a stock assignment") {
		t.Fatalf("Apply = %v, %v", ok, err)
	}
	if st, _ := ReadStatus(status); st.State != "rejected" || !strings.Contains(st.Reason, "not a stock assignment") {
		t.Fatalf("%+v", st)
	}
}

func TestReadStatusOfAMissingFile(t *testing.T) {
	if st, err := ReadStatus(filepath.Join(t.TempDir(), "none.json")); err != nil || st.State != "" {
		t.Fatalf("%+v %v", st, err)
	}
}
