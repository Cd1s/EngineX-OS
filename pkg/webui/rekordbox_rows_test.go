package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

type fullRowsRbxLink struct{ fakeRbxLink }

func (f fullRowsRbxLink) Tracks() ([]prolink.RekordboxTrack, error) {
	rows := make([]prolink.RekordboxTrack, 193)
	for i := range rows {
		rows[i] = prolink.RekordboxTrack{ID: uint32(i + 1), Title: fmt.Sprintf("Unloaded %d", i), Artist: "rekordbox Artist", BPM: 123.45, Key: "Am", DurationMS: int64(211+i) * 1000}
	}
	return rows, nil
}
func (f fullRowsRbxLink) PlaylistTracks(uint32) ([]prolink.RekordboxTrack, error) { return f.Tracks() }

func TestRekordboxUnloadedRowsCarryOwnMetadataInEveryList(t *testing.T) {
	e := newRbxEnv(t, nil)
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return fullRowsRbxLink{}, nil }
	var lists tidalList
	tidalGet(t, e.base, "/v1/users/1/playlists", 200, &lists)
	if len(lists.Items) != 2 {
		t.Fatalf("expected Tracks and nested playlist: %d", len(lists.Items))
	}
	for _, raw := range lists.Items {
		var pl tidalPlaylist
		json.Unmarshal(raw, &pl)
		var rows tidalList
		tidalGet(t, e.base, "/v1/playlists/"+pl.UUID+"/tracks", 200, &rows)
		if len(rows.Items) != 193 {
			t.Fatalf("%s: %d rows", pl.Title, len(rows.Items))
		}
		for i, raw := range rows.Items {
			var row struct {
				ID        int64
				BPM       float64
				Duration  int
				Copyright string
				Artist    struct{ Name string }
			}
			json.Unmarshal(raw, &row)
			if row.BPM != 123.45 || row.Duration != 211+i || row.Copyright != "rekordbox [123.45 BPM 8A]" || row.Artist.Name != "rekordbox Artist" {
				t.Fatalf("%s row %d before load: %+v", pl.Title, i, row)
			}
			if i == 0 {
				var detail struct {
					BPM       float64
					Copyright string
				}
				tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(row.ID, 10), 200, &detail)
				if detail.BPM != 123.45 || detail.Copyright != row.Copyright {
					t.Fatalf("rekordbox metadata cleared from track details: %+v", detail)
				}
			}
		}
	}
}
