package bridge

import (
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestResolveRekordboxTrackPrefersExactCasePath(t *testing.T) {
	first := rekordbox.Track{ID: 60, Path: "/Music/Case/Song.mp3"}
	second := rekordbox.Track{ID: 61, Path: "/Music/case/Song.mp3"}
	byExact := map[string]rekordbox.Track{
		bridgeTrackExactPathKey(first.Path):  first,
		bridgeTrackExactPathKey(second.Path): second,
	}
	byFolded := map[string]rekordbox.Track{bridgeTrackPathKey(first.Path): first}
	got, ok := resolveRekordboxTrack(nil, "../Music/case/Song.mp3", nil, byExact, byFolded)
	if !ok || got.ID != second.ID {
		t.Fatalf("exact case path resolved to id=%d, want %d", got.ID, second.ID)
	}
}

func TestResolveRekordboxTrackUsesPathWhenSourceIDIsUnavailable(t *testing.T) {
	track := rekordbox.Track{ID: 3, Path: "/Music/three.wav"}
	byExact := map[string]rekordbox.Track{bridgeTrackExactPathKey(track.Path): track}
	byFolded := map[string]rekordbox.Track{bridgeTrackPathKey(track.Path): track}
	got, ok := resolveRekordboxTrack(nil, "../Music/three.wav", nil, byExact, byFolded)
	if !ok || got.ID != track.ID {
		t.Fatalf("path fallback resolved to id=%d, want %d", got.ID, track.ID)
	}
}
