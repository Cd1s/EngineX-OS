package enginedb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
)

func TestApplyPendingTrackMetadata(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO Track(id, path, filename, genre, comment, label) VALUES(2, '../Music/song.mp3', 'song.mp3', 'Old Genre', 'Old Comment', 'Old Label')`); err != nil {
		t.Fatal(err)
	}
	if err := QueuePending(root, func(p *Pending) {
		p.TrackMetadata = append(p.TrackMetadata, PendingTrackMetadata{
			TrackID: 2,
			Genre:   "New Genre",
			Comment: "New Comment",
			Label:   "New Label",
		})
	}); err != nil {
		t.Fatal(err)
	}
	n, err := ApplyPending(context.Background(), root)
	if err != nil || n != 1 {
		t.Fatalf("ApplyPending: n=%d err=%v", n, err)
	}
	var got struct{ genre, comment, label string }
	if err := db.QueryRow(`SELECT genre, comment, label FROM Track WHERE id=2`).Scan(&got.genre, &got.comment, &got.label); err != nil {
		t.Fatal(err)
	}
	if got.genre != "New Genre" || got.comment != "New Comment" || got.label != "New Label" {
		t.Fatalf("metadata = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "Engine Library", "Database2", PendingFile)); !os.IsNotExist(err) {
		t.Fatalf("pending queue remains: %v", err)
	}
}

func TestApplyPendingKeepsMetadataForMissingTrack(t *testing.T) {
	_, root, db := testlibrary.Create(t)
	defer db.Close()
	if err := QueuePending(root, func(p *Pending) {
		p.TrackMetadata = append(p.TrackMetadata, PendingTrackMetadata{
			TrackID: 404,
			Genre:   "Still queued",
			Comment: "Media absent",
			Label:   "Later",
		})
	}); err != nil {
		t.Fatal(err)
	}
	n, err := ApplyPending(context.Background(), root)
	if err != nil || n != 0 {
		t.Fatalf("ApplyPending: n=%d err=%v", n, err)
	}
	queued, err := os.ReadFile(filepath.Join(root, "Engine Library", "Database2", PendingFile))
	if err != nil {
		t.Fatal(err)
	}
	var p Pending
	if err := json.Unmarshal(queued, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.TrackMetadata) != 1 || p.TrackMetadata[0].TrackID != 404 {
		t.Fatalf("missing-track metadata was dropped: %+v", p)
	}
}
