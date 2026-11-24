package bridge_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

type issue321Log struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *issue321Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *issue321Log) lines(sub string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, s := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(s, sub) {
			out = append(out, s)
		}
	}
	l.b.Reset()
	return out
}

var issue321Line = regexp.MustCompile(`rb-bridge: cue write-back of \S+ done: (\d+) tracks, (\d+) read, (\d+) changed; ms: candidates \d+, key (\d+), decrypt \d+, edit \d+, encrypt \d+, write \d+, total \d+; (\d+) files, (\d+) bytes written; pass: (manual|watcher|eject)(, first record of the stick)?$`)

// #321: every pass logs one line with the time of each phase, what it read and what it wrote.
func TestIssue321OneLogLinePerPass(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatal(err)
	}
	var out issue321Log
	log.SetOutput(&out)
	defer log.SetOutput(os.Stderr)
	injectDeckCueEdit(t, stick)
	if _, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil {
		t.Fatal(err)
	}
	lines := out.lines("cue write-back of")
	if len(lines) != 1 {
		t.Fatalf("%d lines for one pass: %q", len(lines), lines)
	}
	m := issue321Line.FindStringSubmatch(lines[0])
	if m == nil || m[1] != "2" || m[3] != "1" || m[5] == "0" || m[6] == "0" {
		t.Fatalf("pass that wrote a track and the export: %q", lines[0])
	}
	if _, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", SkipPreparingCheck: true}); err != nil {
		t.Fatal(err)
	}
	lines = out.lines("cue write-back of")
	if len(lines) != 1 {
		t.Fatalf("%d lines for one pass: %q", len(lines), lines)
	}
	if m = issue321Line.FindStringSubmatch(lines[0]); m == nil || m[3] != "0" || m[5] != "0" || m[6] != "0" || m[7] != "manual" || m[8] != "" {
		t.Fatalf("pass with nothing to write: %q", lines[0])
	}

	// #321 review R2: the line names the pass (the Eject's here) and the first record of a stick
	// an earlier build imported (no BridgeSynced), which reads every row once.
	sdb := dbOpen(t, stick)
	if _, err := sdb.Exec(`DROP TABLE bridge.BridgeSynced`); err != nil {
		t.Fatal(err)
	}
	sdb.Close()
	if _, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{Keys: testKeys, DateDir: "2026-11-22", OnEject: true}); err != nil {
		t.Fatal(err)
	}
	lines = out.lines("cue write-back of")
	if len(lines) != 1 {
		t.Fatalf("%d lines for one pass: %q", len(lines), lines)
	}
	if m = issue321Line.FindStringSubmatch(lines[0]); m == nil || m[2] != "2" || m[7] != "eject" || m[8] == "" {
		t.Fatalf("the Eject's first record of the stick: %q", lines[0])
	}
}
