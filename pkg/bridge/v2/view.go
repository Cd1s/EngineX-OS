package bridge

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The key prefixes of the views a stick can show. A stick with one library has no prefix: its
// lists sit at the root, as before two libraries were read.
const (
	viewMerged     = "merged"
	viewDevice     = "device"
	viewOneLibrary = "onelibrary"
	viewLink       = "link:"

	deviceFolderTitle     = "Device Library"
	oneLibraryFolderTitle = "OneLibrary"
)

// MergedFolderTitle is the title of the merged view's folder in the deck's language.
var MergedFolderTitle = func(lang string) string {
	switch lang {
	case "zh-Hans":
		return "全部（合并）"
	case "zh-Hant":
		return "全部（合併）"
	case "es":
		return "Todo (combinado)"
	}
	return "All (merged)"
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
	i, ok := v.index[key]
	if !ok {
		return nil, false
	}
	return &v.Tracks[i], true
}

// node is a list of one library's tree, by its path of names.
type node struct {
	path    []string // "#n" already added for same-name siblings
	name    string
	folder  bool
	entries []string
}

// hashFields is a short digest of fields, separated so that no two field lists collide.
func hashFields(h hash.Hash, fields ...string) string {
	h.Reset()
	for _, f := range fields {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil)[:10])
}

// rowHash names everything a track's row and staged data are made from in the export. Source
// IDs are left out: rekordbox renumbers them between exports, and a row is the same row.
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

// BuildView joins the libraries of a stick. Tracks are joined by key: a file in both libraries
// is one track, with OneLibrary's fields (it carries the cue rows a new row starts with). With
// two libraries and the layout "folders" the root holds three folders, the merged view, Device
// Library and OneLibrary, each with that view's whole tree; a fixed layout shows one view at
// the root, and "device" or "onelibrary" leaves the other library out altogether. A stick
// with one library ignores the layout. Staged CDJ libraries always get a folder of their own.
func BuildView(libs []SourceLibrary, layout, lang string) (*View, error) {
	var device, one *SourceLibrary
	var links []*SourceLibrary
	for i := range libs {
		switch libs[i].Kind {
		case rbstate.DeviceLibrary:
			device = &libs[i]
		case rbstate.OneLibrary:
			one = &libs[i]
		case rbstate.LinkLibrary:
			links = append(links, &libs[i])
		}
	}
	// rekordbox 7 writes empty root lists into OneLibrary that its Device Library does not have
	// and that repeat the name of a list with tracks: they are not the DJ's.
	if one != nil && device != nil {
		filtered := *one
		filtered.Lib = withoutGhostRootLists(one.Lib, device.Lib)
		one = &filtered
	}
	both := device != nil && one != nil
	if both && layout == rbstate.LayoutDevice {
		one, both = nil, false
	} else if both && layout == rbstate.LayoutOneLibrary {
		device, both = nil, false
	}
	v := &View{Layout: layout, index: map[string]int{}}
	h := sha1.New()
	ids := map[*SourceLibrary]map[uint32]string{} // a library's track ID to its key
	for _, l := range append([]*SourceLibrary{one, device}, links...) {
		if l == nil {
			continue
		}
		keys := make(map[uint32]string, len(l.Lib.Tracks))
		ids[l] = keys
		for i := range l.Lib.Tracks {
			t := &l.Lib.Tracks[i]
			rel, err := compat.CleanRelative(t.Path)
			if err != nil {
				return nil, fmt.Errorf("track %d of %s: %w", t.ID, libraryName(l), err)
			}
			key := rbstate.FoldPath(rel)
			keys[t.ID] = key
			hashOfRecord := t.RecordHash
			if hashOfRecord == "" {
				hashOfRecord = rekordbox.RecordHash(*t)
			}
			ref := rbstate.SourceRef{Kind: l.Kind, Source: l.Source, ID: t.ID, RecordHash: hashOfRecord}
			if at, ok := v.index[key]; ok {
				// The same file again: in the other library, or under a second record of this one
				// (rekordbox can list one file twice). The first record stands for it.
				if !hasRef(v.Tracks[at].Refs, l.Kind, l.Source) {
					v.Tracks[at].Refs = append(v.Tracks[at].Refs, ref)
				}
				continue
			}
			v.index[key] = len(v.Tracks)
			v.Tracks = append(v.Tracks, ViewTrack{Key: key, Rel: rel, Track: *t, Refs: []rbstate.SourceRef{ref}, Hash: rowHash(h, t)})
		}
	}
	add := func(prefix string, nodes []node) {
		for _, n := range nodes {
			key := prefix + "\x00" + strings.Join(n.path, "\x00")
			parent := prefix
			if len(n.path) > 1 {
				parent = prefix + "\x00" + strings.Join(n.path[:len(n.path)-1], "\x00")
			}
			v.Lists = append(v.Lists, ViewList{Key: key, Parent: parent, Name: n.name, Folder: n.folder, Tracks: n.entries})
		}
	}
	folder := func(key, title string) {
		v.Lists = append(v.Lists, ViewList{Key: key, Name: title, Folder: true})
	}
	switch {
	case both && layout == rbstate.LayoutMerged:
		add("", mergeTrees(tree(one, ids[one]), tree(device, ids[device])))
	case both:
		folder(viewMerged, MergedFolderTitle(lang))
		add(viewMerged, mergeTrees(tree(one, ids[one]), tree(device, ids[device])))
		folder(viewDevice, deviceFolderTitle)
		add(viewDevice, tree(device, ids[device]))
		folder(viewOneLibrary, oneLibraryFolderTitle)
		add(viewOneLibrary, tree(one, ids[one]))
	case one != nil:
		add("", tree(one, ids[one]))
	case device != nil:
		add("", tree(device, ids[device]))
	}
	for _, l := range links {
		folder(viewLink+l.Source, "[Pro DJ Link: "+l.Source+"]")
		add(viewLink+l.Source, tree(l, ids[l]))
	}
	last := map[string]string{} // a folder's key to its last child so far
	for i := range v.Lists {
		l := &v.Lists[i]
		l.After = last[l.Parent]
		last[l.Parent] = l.Key
		folderFlag := "list"
		if l.Folder {
			folderFlag = "folder"
		}
		l.Hash = hashFields(h, l.Name, l.Parent, l.After, folderFlag) + ":" + hashFields(h, l.Tracks...)
	}
	return v, nil
}

func libraryName(l *SourceLibrary) string {
	switch l.Kind {
	case rbstate.DeviceLibrary:
		return "export.pdb"
	case rbstate.OneLibrary:
		return "exportLibrary.db"
	}
	return "the staged library " + l.Source
}

func hasRef(refs []rbstate.SourceRef, kind rbstate.LibraryKind, source string) bool {
	for _, r := range refs {
		if r.Kind == kind && r.Source == source {
			return true
		}
	}
	return false
}

// tree is a library's lists in display order, a folder before its children. A list's entries
// are the keys of its tracks, each once (Engine keeps a track once in a list); an entry whose
// track the library does not have is left out.
func tree(l *SourceLibrary, keys map[uint32]string) []node {
	if l == nil {
		return nil
	}
	children := map[uint32][]*rekordbox.Playlist{}
	known := map[uint32]bool{}
	for i := range l.Lib.Playlists {
		known[l.Lib.Playlists[i].ID] = true
	}
	for i := range l.Lib.Playlists {
		p := &l.Lib.Playlists[i]
		parent := p.ParentID
		if !known[parent] {
			parent = 0 // a list whose folder is gone shows at the root
		}
		children[parent] = append(children[parent], p)
	}
	var out []node
	seen := map[uint32]bool{}
	var walk func(parent uint32, prefix []string)
	walk = func(parent uint32, prefix []string) {
		used := map[string]int{}
		for _, p := range children[parent] {
			if seen[p.ID] {
				continue // a cycle in the export's tree
			}
			seen[p.ID] = true
			used[p.Name]++
			name := p.Name
			if n := used[p.Name]; n > 1 {
				name = fmt.Sprintf("%s#%d", p.Name, n)
			}
			n := node{path: append(append([]string(nil), prefix...), name), name: p.Name, folder: p.Folder}
			if !p.Folder {
				in := map[string]bool{}
				for _, id := range p.Tracks {
					if key, ok := keys[id]; ok && !in[key] {
						in[key] = true
						n.entries = append(n.entries, key)
					}
				}
			}
			out = append(out, n)
			walk(p.ID, n.path)
		}
	}
	walk(0, nil)
	return out
}

// mergeTrees joins two trees: a list both have, by its folder path and name, is one list. It
// takes the order of whichever has more entries and then what only the other has. Lists only
// b has follow a's among their siblings.
func mergeTrees(a, b []node) []node {
	id := func(n *node) string { return strings.Join(n.path, "\x00") }
	at := map[string]int{}
	out := make([]node, 0, len(a)+len(b))
	for _, n := range a {
		at[id(&n)] = len(out)
		out = append(out, n)
	}
	var extra []node
	for _, n := range b {
		i, ok := at[id(&n)]
		if !ok {
			extra = append(extra, n)
			continue
		}
		first, second := out[i].entries, n.entries
		if len(second) > len(first) {
			first, second = second, first
		}
		merged := append([]string(nil), first...)
		in := make(map[string]bool, len(first))
		for _, k := range first {
			in[k] = true
		}
		for _, k := range second {
			if !in[k] {
				in[k] = true
				merged = append(merged, k)
			}
		}
		out[i].entries = merged
	}
	// A list only b has goes after the last list of the same folder; its own children follow it.
	for _, n := range extra {
		parent := strings.Join(n.path[:len(n.path)-1], "\x00")
		pos := len(out)
		for i := len(out) - 1; i >= 0; i-- {
			p := strings.Join(out[i].path, "\x00")
			if p == parent || strings.HasPrefix(p, parent+"\x00") || parent == "" {
				pos = i + 1
				break
			}
		}
		out = append(out, node{})
		copy(out[pos+1:], out[pos:])
		out[pos] = n
	}
	return out
}

// withoutGhostRootLists returns OneLibrary without the empty root lists rekordbox 7 writes into
// it: an empty list at the root whose name a list with tracks also has, and which the Device
// Library of the same stick does not have as an empty root list.
func withoutGhostRootLists(oneLibrary, pdb *rekordbox.Library) *rekordbox.Library {
	populated := map[string]bool{}
	for _, p := range oneLibrary.Playlists {
		if !p.Folder && len(p.Tracks) > 0 {
			populated[p.Name] = true
		}
	}
	pdbEmptyRoot := map[string]bool{}
	for _, p := range pdb.Playlists {
		if !p.Folder && p.ParentID == 0 && len(p.Tracks) == 0 {
			pdbEmptyRoot[p.Name] = true
		}
	}
	filtered := *oneLibrary
	filtered.Playlists = make([]rekordbox.Playlist, 0, len(oneLibrary.Playlists))
	for _, p := range oneLibrary.Playlists {
		ghost := !p.Folder && p.ParentID == 0 && len(p.Tracks) == 0 && populated[p.Name] && !pdbEmptyRoot[p.Name]
		if !ghost {
			filtered.Playlists = append(filtered.Playlists, p)
		}
	}
	return &filtered
}
