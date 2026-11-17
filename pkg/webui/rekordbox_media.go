package webui

// The rekordbox exports on the USB and SD of the Pioneer players on the link (a CDJ-3000's
// media), in the same source as the computer: each shows as "<player> #<number> USB" (or
// SD) with its tracks and playlists, read from its export.pdb over the player's NFS, and a
// track loads like the computer's, with its analysis from the export's own ANLZ files.
//
// A library is named by a key in the rows' ids: the computer's address, or a player's
// address and the slot, "<ip>/3" for USB and "<ip>/2" for SD.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// rbxPlayerNFS reaches a player's NFS server. A variable for the tests.
var rbxPlayerNFS = func(ip net.IP) *prolink.NFSClient { return &prolink.NFSClient{IP: ip} }

// rbxMediaLibs keeps each medium's parsed export.pdb until the file changes.
var rbxMediaLibs = struct {
	sync.Mutex
	m map[string]rbxMediaLib
}{m: map[string]rbxMediaLib{}}

type rbxMediaLib struct {
	stamp  string
	lib    *rekordbox.Library
	tracks map[uint32]*rekordbox.Track
}

func rbxMediaKey(ip string, slot byte) string { return ip + "/" + strconv.Itoa(int(slot)) }

// rbxMedia splits a library key into the address and the player's slot (0: the computer).
func rbxMedia(key string) (ip string, slot byte) {
	ip, s, ok := strings.Cut(key, "/")
	if !ok {
		return key, 0
	}
	n, _ := strconv.Atoi(s)
	return ip, byte(n)
}

// rbxPlayers are the Pioneer players on the link, while Pro DJ Link is on.
func (s *Server) rbxPlayers() []prolink.Device {
	s.uiMu.Lock()
	on := s.loadUI().ProlinkShare
	s.uiMu.Unlock()
	if !on {
		return nil
	}
	var out []prolink.Device
	seen := map[string]bool{}
	for _, d := range s.mergedDevices() {
		name := strings.ToUpper(d.Name)
		if d.Number < 1 || d.Number > 6 || prolink.IsRekordbox(d) || !strings.HasPrefix(name, "CDJ") && !strings.HasPrefix(name, "XDJ") ||
			time.Since(d.LastSeen) > prolink.DeviceTimeout || net.ParseIP(d.Address) == nil || seen[d.Address] {
			continue
		}
		seen[d.Address] = true
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// rbxDial opens a library: a dbserver session with the computer, or a player's medium.
func (s *Server) rbxDial(ctx context.Context, key string) (rbxLink, error) {
	ip, slot := rbxMedia(key)
	if slot != 0 {
		return openMedia(ctx, net.ParseIP(ip), slot)
	}
	return dialRekordbox(ctx, net.ParseIP(ip), deckPlayer(s))
}

// mediaLink is a player's medium read through its rekordbox export.
type mediaLink struct {
	ctx    context.Context
	nfs    *prolink.NFSClient
	export string
	rbxMediaLib
}

func openMedia(ctx context.Context, ip net.IP, slot byte) (*mediaLink, error) {
	export, err := prolink.SlotExport(slot)
	if err != nil || ip == nil {
		return nil, errors.New("prolink: no such medium")
	}
	c := rbxPlayerNFS(ip)
	key := rbxMediaKey(ip.String(), slot)
	for _, name := range []string{"/PIONEER/rekordbox/export.pdb", "/.PIONEER/rekordbox/export.pdb"} {
		_, f, err := c.Lookup(ctx, export, name)
		if err != nil {
			continue
		}
		stamp := fmt.Sprintf("%s %d %d", name, f.Size, f.MTime)
		rbxMediaLibs.Lock()
		lib := rbxMediaLibs.m[key]
		rbxMediaLibs.Unlock()
		if lib.stamp != stamp {
			b, _, err := c.Fetch(ctx, export, name, 64<<20)
			if err != nil {
				return nil, err
			}
			parsed, err := rekordbox.ParsePDB(bytes.NewReader(b), int64(len(b)))
			if err != nil {
				return nil, err
			}
			lib = rbxMediaLib{stamp: stamp, lib: parsed, tracks: map[uint32]*rekordbox.Track{}}
			for i := range parsed.Tracks {
				lib.tracks[parsed.Tracks[i].ID] = &parsed.Tracks[i]
			}
			rbxMediaLibs.Lock()
			if len(rbxMediaLibs.m) >= 8 {
				clear(rbxMediaLibs.m)
			}
			rbxMediaLibs.m[key] = lib
			rbxMediaLibs.Unlock()
		}
		return &mediaLink{ctx: ctx, nfs: c, export: export, rbxMediaLib: lib}, nil
	}
	return nil, errors.New("prolink: no rekordbox export on the player's medium")
}

func (l *mediaLink) Close() {}

func (l *mediaLink) row(t *rekordbox.Track) prolink.RekordboxTrack {
	return prolink.RekordboxTrack{ID: t.ID, Title: t.Title, Artist: t.Artist, BPM: t.BPM, Key: t.Key, DurationMS: int64(t.Duration * 1000)}
}

func (l *mediaLink) Tracks() ([]prolink.RekordboxTrack, error) {
	rows := make([]prolink.RekordboxTrack, 0, len(l.lib.Tracks))
	for i := range l.lib.Tracks {
		rows = append(rows, l.row(&l.lib.Tracks[i]))
	}
	return rows, nil
}

func (l *mediaLink) Playlists(folder uint32) ([]prolink.RekordboxItem, error) {
	var lists []rekordbox.Playlist
	for _, p := range l.lib.Playlists {
		if p.ParentID == folder {
			lists = append(lists, p)
		}
	}
	sort.SliceStable(lists, func(i, j int) bool { return lists[i].SortOrder < lists[j].SortOrder })
	items := make([]prolink.RekordboxItem, 0, len(lists))
	for _, p := range lists {
		kind := uint32(prolink.RekordboxPlaylist)
		if p.Folder {
			kind = prolink.RekordboxFolder
		}
		items = append(items, prolink.RekordboxItem{Parent: folder, ID: p.ID, Kind: kind, Label: p.Name})
	}
	return items, nil
}

func (l *mediaLink) PlaylistTracks(id uint32) ([]prolink.RekordboxTrack, error) {
	for _, p := range l.lib.Playlists {
		if p.ID == id && !p.Folder {
			rows := make([]prolink.RekordboxTrack, 0, len(p.Tracks))
			for _, tid := range p.Tracks {
				if t := l.tracks[tid]; t != nil {
					rows = append(rows, l.row(t))
				}
			}
			return rows, nil
		}
	}
	return nil, errors.New("prolink: no such playlist")
}

func (l *mediaLink) Path(id uint32) (string, error) {
	if t := l.tracks[id]; t != nil && t.Path != "" {
		return t.Path, nil
	}
	return "", errors.New("prolink: no such track")
}

// Metadata gives the rows a dbserver would: kind 4 title, 7 artist, 2 album, 0xb length in
// seconds, 0xd tempo times 100, 0xf key.
func (l *mediaLink) Metadata(id uint32) ([]prolink.RekordboxItem, error) {
	t := l.tracks[id]
	if t == nil {
		return nil, errors.New("prolink: no such track")
	}
	return []prolink.RekordboxItem{{ID: t.ID, Kind: 4, Label: t.Title}, {Kind: 7, Label: t.Artist}, {Kind: 2, Label: t.Album},
		{Kind: 0xb, ID: uint32(t.Duration + 0.5)}, {Kind: 0xd, ID: uint32(t.BPM*100 + 0.5)}, {Kind: 0xf, Label: t.Key}}, nil
}

// AnalysisOf reads the track's ANLZ files (DAT, EXT, 2EX) from the medium.
func (l *mediaLink) AnalysisOf(id uint32) *rekordbox.Analysis {
	a := &rekordbox.Analysis{}
	t := l.tracks[id]
	if t == nil {
		return a
	}
	p := rbxPath(t.AnalysisPath)
	if !strings.Contains(strings.ToUpper(p), "/USBANLZ/") {
		return a
	}
	base := strings.TrimSuffix(p, path.Ext(p))
	for _, ext := range []string{".DAT", ".EXT", ".2EX"} {
		b, _, err := l.nfs.Fetch(l.ctx, l.export, base+ext, 16<<20)
		if err != nil {
			continue
		}
		if s, err := rekordbox.ParseAnalysis(b); err == nil {
			a.Merge(s)
		}
	}
	return a
}

// rbxMediaName is how a player's medium shows in the list: "CDJ-3000 #2 USB".
func rbxMediaName(d prolink.Device, slot byte) string {
	where := "USB"
	if slot == 2 {
		where = "SD"
	}
	return fmt.Sprintf("%s #%d %s", d.Name, d.Number, where)
}
