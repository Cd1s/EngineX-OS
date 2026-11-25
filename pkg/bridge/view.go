package bridge

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The key prefixes of the views a stick can show. A stick with one library has none: its lists
// sit at the root.
const (
	viewMerged     = "merged"
	viewDevice     = "device"
	viewOneLibrary = "onelibrary"
	viewLink       = "link:"
)

// mergedFolderTitle is the merged view's folder title in the deck's language: the strings of the
// setting's own entry in the deck's Profile, so the folder and the setting read alike.
func mergedFolderTitle(lang string) string {
	if t, ok := map[string]string{"zh-Hans": "合并曲库", "zh-Hant": "合併曲庫", "es": "Biblioteca combinada"}[lang]; ok {
		return t
	}
	return "Merged Library"
}

// ViewTrack is one audio file of the stick, whichever libraries list it.
type ViewTrack struct {
	Key   string              // rbstate.FoldPath of the volume-relative path
	Rel   string              // the path as the export spells it, volume-relative
	Track rekordbox.Track     // the fields the row is written from
	Refs  []rbstate.SourceRef // every library that holds the track
	Hash  string              // of the fields above, without source IDs
}

// ViewList is one playlist or folder as the deck shows it.
type ViewList struct {
	Key    string
	Parent string // the Key of its folder, "" at the root
	After  string // the Key of the sibling before it, "" for the first
	Name   string
	Folder bool
	Tracks []string // keys in order, each once
	Hash   string   // "<title, parent, place>:<entries>"
}

// View is the library the deck shows for a set of exports.
type View struct {
	Stamp   string
	Layout  string
	Sources []rbstate.FileSig
	Tracks  []ViewTrack // every track once, in the order the libraries list them
	Lists   []ViewList  // a folder before its children, siblings in display order
	index   map[string]int
}

// Track returns the view's track of a key.
func (v *View) Track(key string) (*ViewTrack, bool) {
	if i, ok := v.index[key]; ok {
		return &v.Tracks[i], true
	}
	return nil, false
}

// node is a list of one library's tree, by its path of names ("#n" added for same-name siblings).
type node struct {
	path    []string
	name    string
	folder  bool
	entries []string
}

func (n *node) id() string { return strings.Join(n.path, "\x00") }

// hashFields is a short digest of fields, separated so that no two field lists collide. Hashes
// are kept in snapshots on sticks: changing how one is made rewrites every row once.
func hashFields(h hash.Hash, fields ...string) string {
	h.Reset()
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil)[:10])
}

// rowHash names everything a track's row and staged data are made from in the export, without
// source IDs: rekordbox renumbers them between exports.
func rowHash(h hash.Hash, t *rekordbox.Track) string {
	fields := []string{t.Path, t.AnalysisPath, t.Title, t.Artist, t.Album, t.Genre, t.Label, t.Composer, t.Remixer,
		t.Comment, t.Key, t.ArtworkPath,
		strconv.FormatFloat(t.BPM, 'g', -1, 64), strconv.FormatFloat(t.Duration, 'g', -1, 64),
		strconv.FormatUint(t.SampleCount, 10), strconv.Itoa(t.SampleRate), strconv.Itoa(t.BitDepth), strconv.Itoa(t.Bitrate),
		strconv.Itoa(t.Year), strconv.Itoa(t.Rating), strconv.FormatInt(t.FileSize, 10), strconv.Itoa(t.StartOffsetSamples)}
	for _, c := range t.Cues {
		fields = append(fields, fmt.Sprint(c))
	}
	return hashFields(h, fields...)
}

// BuildView joins the libraries of a stick by key: a file in both is one track, with OneLibrary's
// fields (it carries the cue rows a new row starts with). With two libraries and the layout
// "folders" the root holds three folders, the merged view, Device Library and OneLibrary, each
// with that view's whole tree; "merged" shows the merged view at the root, and "device" or
// "onelibrary" leaves the other library out. Staged CDJ libraries get a folder each (#59).
func BuildView(libs []SourceLibrary, layout, lang string) (*View, error) {
	return BuildViewCtx(context.Background(), libs, layout, lang)
}

func BuildViewCtx(ctx context.Context, libs []SourceLibrary, layout, lang string) (*View, error) {
	var device, one *SourceLibrary
	var links []*SourceLibrary
	for i := range libs {
		switch libs[i].Kind {
		case rbstate.DeviceLibrary:
			device = &libs[i]
		case rbstate.OneLibrary:
			one = &libs[i]
		default:
			links = append(links, &libs[i])
		}
	}
	if one != nil && device != nil {
		filtered := *one
		filtered.Lib = withoutGhostRootLists(one.Lib, device.Lib)
		one = &filtered
		if layout == rbstate.LayoutDevice {
			one = nil
		} else if layout == rbstate.LayoutOneLibrary {
			device = nil
		}
	}
	v := &View{Layout: layout, index: map[string]int{}}
	h := sha1.New()
	keys := map[*SourceLibrary]map[uint32]string{} // a library's track ID to its key
	for _, l := range append([]*SourceLibrary{one, device}, links...) {
		if l == nil {
			continue
		}
		keys[l] = make(map[uint32]string, len(l.Lib.Tracks))
		for i := range l.Lib.Tracks {
			if i&511 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			t := &l.Lib.Tracks[i]
			rel, err := compat.CleanRelative(t.Path)
			if err != nil {
				return nil, fmt.Errorf("track %d of library %d %s: %w", t.ID, l.Kind, l.Source, err)
			}
			key := rbstate.FoldPath(rel)
			keys[l][t.ID] = key
			ref := rbstate.SourceRef{Kind: l.Kind, Source: l.Source, ID: t.ID, RecordHash: t.RecordHash}
			if ref.RecordHash == "" {
				ref.RecordHash = rekordbox.RecordHash(*t)
			}
			// The same file again, in the other library or under a second record of this one: the
			// first record stands for it.
			if at, ok := v.index[key]; !ok {
				v.index[key] = len(v.Tracks)
				v.Tracks = append(v.Tracks, ViewTrack{Key: key, Rel: rel, Track: *t, Refs: []rbstate.SourceRef{ref}, Hash: rowHash(h, t)})
			} else if !slices.ContainsFunc(v.Tracks[at].Refs, func(r rbstate.SourceRef) bool { return r.Kind == l.Kind && r.Source == l.Source }) {
				v.Tracks[at].Refs = append(v.Tracks[at].Refs, ref)
			}
		}
	}
	add := func(prefix string, nodes []node) {
		for _, n := range nodes {
			parent := prefix
			if len(n.path) > 1 {
				parent += "\x00" + strings.Join(n.path[:len(n.path)-1], "\x00")
			}
			v.Lists = append(v.Lists, ViewList{Key: prefix + "\x00" + n.id(), Parent: parent, Name: n.name, Folder: n.folder, Tracks: n.entries})
		}
	}
	folder := func(key, title string, nodes []node) {
		v.Lists = append(v.Lists, ViewList{Key: key, Name: title, Folder: true})
		add(key, nodes)
	}
	switch {
	case one != nil && device != nil && layout == rbstate.LayoutMerged:
		add("", mergeTrees(tree(one, keys[one]), tree(device, keys[device])))
	case one != nil && device != nil:
		folder(viewMerged, mergedFolderTitle(lang), mergeTrees(tree(one, keys[one]), tree(device, keys[device])))
		folder(viewDevice, "Device Library", tree(device, keys[device]))
		folder(viewOneLibrary, "OneLibrary", tree(one, keys[one]))
	case one != nil:
		add("", tree(one, keys[one]))
	case device != nil:
		add("", tree(device, keys[device]))
	}
	for _, l := range links {
		folder(viewLink+l.Source, "[Pro DJ Link: "+l.Source+"]", tree(l, keys[l]))
	}
	last := map[string]string{} // a folder's key to its last child so far
	for i := range v.Lists {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		l := &v.Lists[i]
		l.After, last[l.Parent] = last[l.Parent], l.Key
		kind := "list"
		if l.Folder {
			kind = "folder"
		}
		l.Hash = hashFields(h, l.Name, l.Parent, l.After, kind) + ":" + hashFields(h, l.Tracks...)
	}
	return v, nil
}

// tree is a library's lists in display order, a folder before its children. A list's entries are
// the keys of its tracks, each once (Engine keeps a track once in a list), without tracks the
// library does not have. A list whose folder is gone shows at the root.
func tree(l *SourceLibrary, keys map[uint32]string) []node {
	children := map[uint32][]*rekordbox.Playlist{}
	known := map[uint32]bool{}
	for i := range l.Lib.Playlists {
		known[l.Lib.Playlists[i].ID] = true
	}
	for i := range l.Lib.Playlists {
		p := &l.Lib.Playlists[i]
		parent := p.ParentID
		if !known[parent] {
			parent = 0
		}
		children[parent] = append(children[parent], p)
	}
	var out []node
	seen := map[uint32]bool{} // against a cycle in the export's tree
	var walk func(parent uint32, prefix []string)
	walk = func(parent uint32, prefix []string) {
		used := map[string]int{}
		for _, p := range children[parent] {
			if seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			name := p.Name
			if used[p.Name]++; used[p.Name] > 1 {
				name = fmt.Sprintf("%s#%d", p.Name, used[p.Name])
			}
			n := node{path: append(slices.Clone(prefix), name), name: p.Name, folder: p.Folder}
			if !p.Folder {
				var ids []string
				for _, id := range p.Tracks {
					if key, ok := keys[id]; ok {
						ids = append(ids, key)
					}
				}
				n.entries = union(nil, ids)
			}
			out = append(out, n)
			walk(p.ID, n.path)
		}
	}
	walk(0, nil)
	return out
}

// union is a followed by what b has that a has not, each key once.
func union(a, b []string) []string {
	var out []string
	in := map[string]bool{}
	for _, k := range slices.Concat(a, b) {
		if !in[k] {
			in[k] = true
			out = append(out, k)
		}
	}
	return out
}

// mergeTrees joins two trees: a list both have, by folder path and name, is one list with the
// order of whichever has more entries, then what only the other has. A list only b has goes after
// the last list of its folder, its own children after it.
func mergeTrees(a, b []node) []node {
	at := map[string]int{}
	out := slices.Clone(a)
	for i := range out {
		at[out[i].id()] = i
	}
	var extra []node
	for _, n := range b {
		i, ok := at[n.id()]
		if !ok {
			extra = append(extra, n)
		} else if len(n.entries) > len(out[i].entries) {
			out[i].entries = union(n.entries, out[i].entries)
		} else {
			out[i].entries = union(out[i].entries, n.entries)
		}
	}
	for _, n := range extra {
		parent := strings.Join(n.path[:len(n.path)-1], "\x00")
		pos := len(out)
		for i := len(out) - 1; i >= 0 && parent != ""; i-- {
			if p := out[i].id(); p == parent || strings.HasPrefix(p, parent+"\x00") {
				pos = i + 1
				break
			}
		}
		out = slices.Insert(out, pos, n)
	}
	return out
}

// withoutGhostRootLists leaves out of OneLibrary the empty root lists rekordbox 7 writes into it:
// empty, at the root, named like a list with tracks, and not an empty root list of the Device
// Library of the same stick.
func withoutGhostRootLists(oneLibrary, pdb *rekordbox.Library) *rekordbox.Library {
	emptyRoot := func(p *rekordbox.Playlist) bool { return !p.Folder && p.ParentID == 0 && len(p.Tracks) == 0 }
	populated, pdbEmpty := map[string]bool{}, map[string]bool{}
	for _, p := range oneLibrary.Playlists {
		populated[p.Name] = populated[p.Name] || !p.Folder && len(p.Tracks) > 0
	}
	for i := range pdb.Playlists {
		pdbEmpty[pdb.Playlists[i].Name] = pdbEmpty[pdb.Playlists[i].Name] || emptyRoot(&pdb.Playlists[i])
	}
	filtered := *oneLibrary
	filtered.Playlists = slices.DeleteFunc(slices.Clone(oneLibrary.Playlists), func(p rekordbox.Playlist) bool {
		return emptyRoot(&p) && populated[p.Name] && !pdbEmpty[p.Name]
	})
	return &filtered
}
