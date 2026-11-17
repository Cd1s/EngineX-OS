package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
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
	if d := time.Since(start); d > 500*time.Millisecond {
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

func TestProlinkSettingOnMigration(t *testing.T) {
	for _, tc := range []struct {
		name string
		conf string
		want bool
	}{
		{"only_share", "prolink_share=1\n", true},
		{"only_export", "prolink_export=1\n", true},
		{"both", "prolink_share=1\nprolink_export=1\n", true},
		{"neither", "waveform=rgb\n", false},
		{"both_off", "prolink_share=0\nprolink_export=0\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.conf")
			if err := os.WriteFile(path, []byte(tc.conf), 0600); err != nil {
				t.Fatal(err)
			}
			if got := prolinkSettingOn(path); got != tc.want {
				t.Fatalf("prolinkSettingOn(%q) = %v, want %v", tc.conf, got, tc.want)
			}
		})
	}
}

// A deck's journal got every Pro DJ Link packet from rb-bridge (25,660 lines in
// 30 min with a computer on the link): it now gets a device when it appears and
// when it has been silent for 10 s; packets need verbose_log=1.
func TestRBBridgeLogsDevicesNotPackets(t *testing.T) {
	var lines []string
	logf := func(v ...any) { lines = append(lines, fmt.Sprint(v...)) }
	l := linkLog{seen: map[string]time.Time{}}
	now := time.Now()
	computer := prolink.Device{Address: "192.168.1.138", Number: 17, Name: "rekordbox"}
	for i := range 50 {
		l.packet(logf, computer, now.Add(time.Duration(i)*100*time.Millisecond))
	}
	l.packet(logf, prolink.Device{Address: "192.168.1.238", Number: 4, Name: "CDJ-3000"}, now)
	if l.expire(logf, now.Add(10*time.Second)) {
		t.Fatal("published a loss before the timeout")
	}
	if !l.expire(logf, now.Add(12*time.Second)) {
		t.Fatal("CDJ loss did not dirty the snapshot")
	}
	if !l.expire(logf, now.Add(16*time.Second)) {
		t.Fatal("computer loss did not dirty the snapshot")
	}
	if l.expire(logf, now.Add(17*time.Second)) {
		t.Fatal("unchanged snapshot dirtied again")
	}
	want := []string{
		`rb-bridge: Pro DJ Link device seen: 192.168.1.138 player 17 "rekordbox"`,
		`rb-bridge: Pro DJ Link device seen: 192.168.1.238 player 4 "CDJ-3000"`,
		`rb-bridge: Pro DJ Link device lost: 192.168.1.238 player 4 "CDJ-3000"`,
		`rb-bridge: Pro DJ Link device lost: 192.168.1.138 player 17 "rekordbox"`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("journal:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
