package bridge_test

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
)

func TestP3WriteBackLogsOnePassLine(t *testing.T) {
	mount := t.TempDir()
	testfixture.Export(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	p3DeckCue(t, db, p3FirstTrackID(t, db), "logged cue", 1, 1000)
	_ = db.Close()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	if _, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{SkipPreparingCheck: true}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.Contains(line, "cue write-back of") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "1 changed") || !strings.Contains(lines[0], "pass: manual") {
		t.Fatalf("write-back log lines=%v", lines)
	}
}
