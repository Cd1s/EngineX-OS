package rekordbox

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
)

func TestRewritePDBMetadataRetainsUnknownRowsAndVariableStrings(t *testing.T) {
	raw := testfixture.PDB()
	updated, err := RewritePDBMetadata(raw, map[uint32]Metadata{
		1: {
			Genre:   "House",
			Comment: strings.Repeat("备注", 100),
			Label:   strings.Repeat("Label-", 40),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := ParsePDB(bytes.NewReader(updated), int64(len(updated)))
	if err != nil {
		t.Fatal(err)
	}
	var found *Track
	for i := range lib.Tracks {
		if lib.Tracks[i].ID == 1 {
			found = &lib.Tracks[i]
			break
		}
	}
	if found == nil {
		t.Fatal("updated track is missing")
	}
	if found.Genre != "House" || found.Label != strings.Repeat("Label-", 40) || found.Comment != strings.Repeat("备注", 100) {
		t.Fatalf("metadata = %+v", *found)
	}
	if !bytes.Contains(updated, []byte("opaque column definition")) {
		t.Fatal("unknown type-16 row was not retained")
	}
	if len(updated) <= len(raw) {
		t.Fatalf("variable strings did not expand export: before=%d after=%d", len(raw), len(updated))
	}
}
