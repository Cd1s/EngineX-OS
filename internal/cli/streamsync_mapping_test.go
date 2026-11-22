package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/mapping"
)

const mappingStock = `import airAssignments 1.0
import QtQuick 2.12

MidiAssignment {
    GlobalAssignmentConfig {
        id: globalConfig
        midiChannel: 0
    }

    Shift {
        note: 26
    }

    DeckAssignmentConfig {
        id: deckConfig
        midiChannel: globalConfig.midiChannel
    }

    PadModeSelect {
        buttonsModel: ListModel {
            ListElement {
                note: 30
            }
        }
    }
}
`

func TestStreamSyncMappingGenerateWritesThePatchedFile(t *testing.T) {
	dir := t.TempDir()
	in, out, file := filepath.Join(dir, "in.qml"), filepath.Join(dir, "out.qml"), filepath.Join(dir, "mappings.json")
	status := mapping.StatusPath(dir, "JP07")
	os.WriteFile(in, []byte(mappingStock), 0o644)
	os.WriteFile(file, []byte(`[{"board":"JP07","control":"PadModeSelect:30","gesture":"shift","action":"waveform_cycle"}]`), 0o644)
	args := []string{"--mapping-generate", "JP07", "--mapping-in", in, "--mapping-out", out, "--mapping-file", file, "--mapping-status", status}
	if err := StreamSync(args); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), "EngineX Map PadModeSelect:30 shift") {
		t.Fatalf("%v\n%s", err, b)
	}
	if st, _ := mapping.ReadStatus(status); st.State != "applied" {
		t.Fatalf("%+v", st)
	}
	// a rejected file writes nothing and fails, so the guard binds nothing
	os.Remove(out)
	os.WriteFile(file, []byte(`[{"board":"JP07","control":"Shift:26","gesture":"press","action":"sync"}]`), 0o644)
	if err := StreamSync(args); err == nil {
		t.Fatal("a reserved control was applied")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a file was written for a rejected mapping")
	}
	// no mappings.json at all: success, nothing written
	os.Remove(file)
	if err := StreamSync(args); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a file was written with no mappings")
	}
	if err := StreamSync([]string{"--mapping-generate", "JP07"}); err == nil {
		t.Fatal("missing paths were accepted")
	}
}
