package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func TestRBBridgeValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, "specify --mount-path or --prolink"},
		{[]string{"--prolink", "--bind=invalid"}, "bind must be IPv4"},
		{[]string{"--mount-path=/missing-library", "--prolink", "--bind=::1"}, "bind must be IPv4"},
		{[]string{"--mount-path=/x", "extra"}, "unexpected positional"},
	} {
		err := RBBridge(tc.args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v, want %s", tc.args, err, tc.want)
		}
	}
}

func TestRBBridgeRetiredWatchExitsAtOnce(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	// The unit in the player's base /etc runs "--watch --prolink".
	for _, args := range [][]string{{"--watch", "--prolink"}, {"--watch", "--watch-roots", root, "--poll-interval", "1ms"}} {
		var out bytes.Buffer
		start := time.Now()
		if err := RBBridge(args, &out); err != nil || out.Len() != 0 || time.Since(start) > time.Second {
			t.Fatalf("%v: %v %q %s", args, err, &out, time.Since(start))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library")); !os.IsNotExist(err) {
		t.Fatal("retired watch imported a volume")
	}
}

// An unchanged export is recognised from its own size and time alone: the
// second run must not need the tracks or analysis files at all.
func TestRBBridgeUnchangedExportSkipsTheVolume(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	run := func() enginedb.Result {
		t.Helper()
		var out bytes.Buffer
		if err := RBBridge([]string{"--mount-path", root}, &out); err != nil {
			t.Fatal(err)
		}
		var r enginedb.Result
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatalf("%q: %v", &out, err)
		}
		return r
	}
	if r := run(); r.Bypassed || r.Added == 0 {
		t.Fatal(r)
	}
	db := filepath.Join(root, "Engine Library", "Database2", "m.db")
	before, _ := os.ReadFile(db)
	if err := os.RemoveAll(filepath.Join(root, "PIONEER", "USBANLZ")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r := run(); !r.Bypassed {
		t.Fatal(r)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("unchanged export took %s", d)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Fatal("unchanged export rewrote m.db")
	}
}

func TestRBBridgeProlinkFlags(t *testing.T) {
	t.Setenv("ENGINEX_PROLINK_STATE", "/tmp/custom-state.json")
	var out bytes.Buffer
	err := RBBridge([]string{"--prolink", "--state-file=/custom.json", "--anlz-root=/custom-anlz", "--bind=invalid"}, &out)
	if err == nil || !strings.Contains(err.Error(), "bind must be IPv4") {
		t.Fatalf("expected bind error, got %v", err)
	}
}
