package bridge

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestWriteOverviewRepairsInvalidExistingRGB(t *testing.T) {
	dir := t.TempDir()
	uuid := "review-uuid"
	path := filepath.Join(dir, "OverviewData", uuid, "1.rgb")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt payload that is too large"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 30)
	binary.BigEndian.PutUint64(raw[0:], 1)
	binary.BigEndian.PutUint64(raw[8:], 1)
	body, err := enginedb.QCompress(raw)
	if err != nil {
		t.Fatal(err)
	}
	writeOverview(dir, uuid, 1, body)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := enginedb.QUncompress(got, 1<<20)
	if err != nil || !enginedb.ValidRGBOverview(decoded) {
		t.Fatalf("RGB was not repaired: valid=%v err=%v", enginedb.ValidRGBOverview(decoded), err)
	}
}
