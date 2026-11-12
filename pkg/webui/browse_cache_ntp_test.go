package webui

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBrowseDiskServesCachedPlaylistsBeforeNTPSync(t *testing.T) {
	dir := t.TempDir()
	key := browseKey("spotify", "owner", "lists", "")

	// Home Wi-Fi session writes playlist cache with synced 2026 clock.
	home := newBrowseCache(dir)
	syncedAt := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	home.now = func() time.Time { return syncedAt }
	if _, err := home.get(context.Background(), key, func(context.Context) (browseData, error) {
		return browseFixture("Club Crate"), nil
	}); err != nil {
		t.Fatal(err)
	}

	// Deck reboots offline at a club (no RTC battery, pre-NTP kernel build epoch in 2024).
	boot := newBrowseCache(dir)
	preNTP := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	boot.now = func() time.Time { return preNTP }
	got, err := boot.get(context.Background(), key, func(context.Context) (browseData, error) {
		return browseData{}, errors.New("network unreachable")
	})
	if f := currentBrowseFlight(boot, key); f != nil {
		waitBrowse(t, f.done)
	}
	if err != nil || len(got.Lists) != 1 || got.Lists[0].Title != "Club Crate" {
		t.Fatalf("offline pre-NTP boot rejected disk-cached playlists: got=%+v err=%v", got, err)
	}
}
