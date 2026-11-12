package tracklist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObserveBackdatedTimestampsDedupAndExports(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(Config{
		StateDir:     dir,
		AudibleHold:  1500 * time.Millisecond,
		FallbackHold: 10 * time.Second,
	})

	t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
	mgr.Start(t0, "Club Night Set", "tracklist_only", "club-set.wav")

	// Track 1 on Player 1 Deck 1 with StageLinQ mixer known: fader up at t0+0s
	c1 := DeckCandidate{
		Player: 1, Deck: 1, Channel: 1, SourceLabel: "Player 1 · Deck 1",
		Artist: "Bicep", Title: "Glue", BPM: 130.0, Key: "8A",
		Playing: true, OnAir: true, MixerKnown: true,
	}
	mgr.ObserveCandidates(t0, []DeckCandidate{c1})
	// At +1.0s (< 1.5s hold): not committed yet
	added, _, _ := mgr.ObserveCandidates(t0.Add(1000*time.Millisecond), []DeckCandidate{c1})
	if len(added) != 0 {
		t.Fatalf("expected 0 added before hold, got %d", len(added))
	}
	// At +1.6s: committed with OffsetSec backdated to 0.0s!
	added, sess, ok := mgr.ObserveCandidates(t0.Add(1600*time.Millisecond), []DeckCandidate{c1})
	if !ok || len(added) != 1 || added[0].OffsetSec != 0 {
		t.Fatalf("expected backdated 0.0s entry, got ok=%v added=%+v", ok, added)
	}

	// Quick fader cut and bring-back of the same track does NOT duplicate
	c1Cut := c1
	c1Cut.OnAir = false
	mgr.ObserveCandidates(t0.Add(15*time.Second), []DeckCandidate{c1Cut})
	mgr.ObserveCandidates(t0.Add(16*time.Second), []DeckCandidate{c1})
	added, _, _ = mgr.ObserveCandidates(t0.Add(18*time.Second), []DeckCandidate{c1})
	if len(added) != 0 {
		t.Fatalf("expected fader tap of same track to be deduplicated, got %+v", added)
	}

	// Track 2 on Player 2 Deck 2 at +3m00s (180s) with analog fallback (MixerKnown=false, 10s hold)
	c2 := DeckCandidate{
		Player: 2, Deck: 2, Channel: 2, SourceLabel: "Player 2 · Deck 2",
		Artist: "Four Tet", Title: "Baby", BPM: 132.5, Key: "9A",
		Playing: true, OnAir: false, MixerKnown: false, Streaming: true,
	}
	mgr.ObserveCandidates(t0.Add(180*time.Second), []DeckCandidate{c2})
	added, _, _ = mgr.ObserveCandidates(t0.Add(189*time.Second), []DeckCandidate{c2})
	if len(added) != 0 {
		t.Fatal("expected analog fallback not to commit at 9s")
	}
	added, sess, _ = mgr.ObserveCandidates(t0.Add(190*time.Second), []DeckCandidate{c2})
	if len(added) != 1 || added[0].OffsetSec != 180.0 {
		t.Fatalf("expected analog fallback backdated to 180.0s, got %+v", added)
	}
	if !sess.StreamingNotice {
		t.Fatal("expected StreamingNotice=true when streaming track entered set")
	}

	// Track 3 at 1h05m30s (3930s) via PeerSync from partner player, plus duplicate check
	peerSess := sess
	peerSess.Entries = append(peerSess.Entries, Entry{
		OffsetSec:   3930.5,
		StartedAt:   t0.Add(3930500 * time.Millisecond),
		Player:      2,
		Deck:        4,
		SourceLabel: "Player 2 · Deck 4",
		Artist:      `Aphex "Twin"`,
		Title:       "Windowlicker",
		BPM:         127.0,
	})
	merged := mgr.ApplyPeerSync(PeerSyncMessage{Action: "sync", Session: peerSess})
	if len(merged.Entries) != 3 {
		t.Fatalf("expected 3 merged entries, got %d", len(merged.Entries))
	}

	done, stopped := mgr.Stop(t0.Add(4200 * time.Second))
	if !stopped || done.Active {
		t.Fatal("expected Stop to finalize session")
	}

	// Verify Plain Text, CUE (>60 min MM=65!), Mixcloud, OBS NowPlaying, and sidecar files
	txt := FormatText(done)
	if !strings.Contains(txt, "01. 00:00:00  Bicep - Glue  [Player 1 · Deck 1 · 130.0 BPM · 8A]") ||
		!strings.Contains(txt, "02. 00:03:00  Four Tet - Baby") ||
		!strings.Contains(txt, "03. 01:05:31  Aphex \"Twin\" - Windowlicker") {
		t.Fatalf("unexpected text export:\n%s", txt)
	}

	cue := FormatCUE(done, "club-set.wav")
	if !strings.Contains(cue, `FILE "club-set.wav" WAVE`) ||
		!strings.Contains(cue, `PERFORMER "Aphex 'Twin'"`) ||
		!strings.Contains(cue, `INDEX 01 65:30:38`) {
		t.Fatalf("unexpected CUE export:\n%s", cue)
	}

	mc := FormatMixcloud(done)
	if !strings.Contains(mc, "00:00 Bicep - Glue\n03:00 Four Tet - Baby\n1:05:31 Aphex \"Twin\" - Windowlicker\n") {
		t.Fatalf("unexpected Mixcloud export:\n%s", mc)
	}
	secs, form := MixcloudSections(done)
	if len(secs) != 3 || form["sections-1-start_time"] != "180" || form["sections-1-artist"] != "Four Tet" {
		t.Fatalf("unexpected Mixcloud sections/form: %+v %+v", secs, form)
	}
	if np := NowPlayingLine(done); np != `Aphex "Twin" - Windowlicker` {
		t.Fatalf("unexpected NowPlayingLine: %q", np)
	}

	for _, ext := range []string{".json", ".txt", ".cue", ".mixcloud.txt"} {
		if _, err := os.Stat(filepath.Join(dir, "tracklists", done.ID+ext)); err != nil {
			t.Fatalf("missing sidecar %s: %v", ext, err)
		}
	}
}
