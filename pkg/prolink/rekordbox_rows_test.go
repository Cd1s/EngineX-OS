package prolink

import (
	"context"
	"fmt"
	"net"
	"testing"
)

func TestRekordboxFullRowsBeforeLoadAcrossMenuPages(t *testing.T) {
	for _, kind := range []uint16{0x1004, 0x1105} {
		t.Run(fmt.Sprintf("menu_%04x", kind), func(t *testing.T) {
			const count = 193
			plain := make([]dbItem, count)
			artist, tempo, duration := make([]dbItem, count), make([]dbItem, count), make([]dbItem, count)
			for i := range plain {
				id := uint32(i + 1)
				plain[i] = dbItem{id: id, kind: 4, label: fmt.Sprintf("Track %d", id)}
				// Every sorted view reorders the IDs; none has been loaded or analysed by Engine.
				j := count - 1 - i
				artist[j] = dbItem{id: id, kind: 0x0704, secondary: fmt.Sprintf("Artist %d", id)}
				tempo[j] = dbItem{id: id, parent: 12345, kind: 0x0d04, secondary: "123.45 bpm - Am"}
				duration[j] = dbItem{id: id, parent: 210 + id, kind: 0x0b04}
			}
			f := newFakeRekordbox(t, map[uint16][]dbItem{kind: plain})
			f.sorted = map[uint32][]dbItem{2: artist, 4: tempo, 8: duration}
			l, err := dialRekordbox(context.Background(), net.IPv4(127, 0, 0, 1), f.port(), 0)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			var rows []RekordboxTrack
			if kind == 0x1004 {
				rows, err = l.Tracks()
			} else {
				rows, err = l.PlaylistTracks(99)
			}
			if err != nil || len(rows) != count {
				t.Fatalf("rows: %d %v", len(rows), err)
			}
			for i, r := range rows {
				if r.ID != uint32(i+1) || r.Artist != fmt.Sprintf("Artist %d", r.ID) || r.BPM != 123.45 || r.Key != "Am" || r.DurationMS != int64(210+r.ID)*1000 {
					t.Fatalf("row %d: %+v", i, r)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			requests, renders := 0, 0
			for _, m := range f.seen {
				if m.kind == 0x2002 {
					t.Fatal("per-track metadata query for a menu column")
				}
				if m.kind == kind {
					requests++
				}
				if m.kind == 0x3000 {
					renders++
				}
			}
			if requests != 4 || renders != 16 {
				t.Fatalf("menu traffic: %d requests, %d page renders", requests, renders)
			}
		})
	}
}
