package bridge_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// #328 stage 1B, cause B: the background job of a first insert said nothing while it read the
// export, resolved its paths and copied the library (its first count came after the snapshot),
// and nothing during a pass (a count only at each pass's end): the banner stood still for
// minutes on a large stick. Now the job reports 0 of the export's tracks once the export is read,
// before those long steps, and each pass's share as it goes; never fewer than before.
func TestIssue328BackgroundJobCountsThroughout(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	root := t.TempDir()
	testfixture.LargeExport(t, root, issue328Tracks)
	defer bridge.SetUpdateExportForTest()()
	ctx := context.Background()
	if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Background {
		t.Fatalf("first mount: %+v %v", r, err)
	}
	// About a track a pass: many passes, each with its own reads.
	defer bridge.SetImportWindowForTest(time.Millisecond)()
	var seq []string
	defer bridge.SetCutForTest(func(name string) { seq = append(seq, "cut "+name) })()
	type report struct{ done, total int }
	var reports []report
	if r, err := bridge.ImportInBackground(ctx, root, func(done, total int) {
		reports = append(reports, report{done, total})
		seq = append(seq, fmt.Sprintf("progress %d/%d", done, total))
	}); err != nil || r.Deferred != 0 {
		t.Fatalf("background update: %+v %v", r, err)
	}
	if len(reports) == 0 || reports[0] != (report{0, issue328Tracks}) {
		t.Fatalf("first report %v, want 0 of %d", reports, issue328Tracks)
	}
	at := func(s string) int {
		for i, x := range seq {
			if x == s {
				return i
			}
		}
		return -1
	}
	if snap := at("cut snapshot copied"); snap < 0 || at(fmt.Sprintf("progress 0/%d", issue328Tracks)) > snap {
		t.Errorf("the first report comes after the copy of the library: %v", seq[:min(len(seq), 8)])
	}
	// During a pass: a count the pass reports before it commits. Before #328 every count past 0
	// came right after a pass's commit.
	during, passes := false, 0
	for i, x := range seq {
		if x == "cut stage pass committed" {
			passes++
		}
		if i > 0 && strings.HasPrefix(x, "progress ") && x != fmt.Sprintf("progress 0/%d", issue328Tracks) && seq[i-1] != "cut stage pass committed" {
			during = true
		}
	}
	if !during {
		t.Errorf("no count during a pass (%d passes): %v", passes, seq[:min(len(seq), 12)])
	}
	for i, r := range reports {
		if r.total != issue328Tracks || i > 0 && r.done < reports[i-1].done {
			t.Fatalf("report %d of %v: %v goes back or is not of the whole count", i, len(reports), r)
		}
	}
	if last := reports[len(reports)-1]; last.done != issue328Tracks {
		t.Fatalf("last report %v, want all %d", last, issue328Tracks)
	}
	t.Logf("%d passes, %d reports", passes, len(reports))
}
