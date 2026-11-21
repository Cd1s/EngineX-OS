package rekordbox

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

type rawPDBTable struct {
	typ        uint32
	descriptor uint32
	header     []byte
	data       []byte
	rows       [][]byte
}

// RewritePDBMetadata updates track genre, label, and comment fields while
// retaining every live row and unknown table from the source DeviceSQL file.
// The page layout is rebuilt because DeviceSQL strings are variable-sized.
func RewritePDBMetadata(raw []byte, updates map[uint32]Metadata) ([]byte, error) {
	if len(updates) == 0 {
		return append([]byte(nil), raw...), nil
	}
	if len(raw) < 28 {
		return nil, fmt.Errorf("pdb header: EOF")
	}
	pageSize := int(binary.LittleEndian.Uint32(raw[4:]))
	tableCount := int(binary.LittleEndian.Uint32(raw[8:]))
	if pageSize < 512 || pageSize > 65536 || len(raw)%pageSize != 0 || tableCount < 0 || tableCount > (pageSize-28)/16 {
		return nil, fmt.Errorf("pdb: invalid page size/table count")
	}
	if len(raw) < pageSize || tableCount == 0 {
		return nil, fmt.Errorf("pdb: truncated header")
	}

	tables := make([]rawPDBTable, 0, tableCount+2)
	for i := 0; i < tableCount; i++ {
		off := 28 + i*16
		typ := binary.LittleEndian.Uint32(raw[off:])
		descriptor := binary.LittleEndian.Uint32(raw[off+4:])
		first := binary.LittleEndian.Uint32(raw[off+8:])
		last := binary.LittleEndian.Uint32(raw[off+12:])
		t := rawPDBTable{typ: typ, descriptor: descriptor}
		seen := map[uint32]bool{}
		for pageNo := first; pageNo != 0; {
			if pageNo >= uint32(len(raw)/pageSize) || seen[pageNo] {
				return nil, fmt.Errorf("pdb: invalid/cyclic page %d", pageNo)
			}
			seen[pageNo] = true
			page := raw[int(pageNo)*pageSize : int(pageNo+1)*pageSize]
			if binary.LittleEndian.Uint32(page[4:]) != pageNo || binary.LittleEndian.Uint32(page[8:]) != typ {
				return nil, fmt.Errorf("pdb: page %d type/index mismatch", pageNo)
			}
			if len(t.header) == 0 {
				t.header = append([]byte(nil), page...)
			}
			count := int(binary.LittleEndian.Uint32(page[24:]) & 0x1fff)
			if page[27]&0x40 == 0 && count > 0 {
				if len(t.data) == 0 {
					t.data = append([]byte(nil), page...)
				}
				rows, err := pdbPageRows(page, pageSize, count)
				if err != nil {
					return nil, err
				}
				t.rows = append(t.rows, rows...)
			}
			if pageNo == last {
				break
			}
			next := binary.LittleEndian.Uint32(page[12:])
			if next == 0 {
				return nil, fmt.Errorf("pdb: page chain ends before last page")
			}
			pageNo = next
		}
		if len(t.header) == 0 {
			return nil, fmt.Errorf("pdb: table %d has no pages", typ)
		}
		tables = append(tables, t)
	}

	genreIndex := findRawPDBTable(tables, 1)
	labelIndex := findRawPDBTable(tables, 4)
	genreIDs, genreMax := rawPDBLookup(tables, genreIndex)
	labelIDs, labelMax := rawPDBLookup(tables, labelIndex)
	changed := false
	for i := range tables {
		if tables[i].typ != 0 {
			continue
		}
		for j, row := range tables[i].rows {
			if len(row) < 136 {
				continue
			}
			trackID := binary.LittleEndian.Uint32(row[72:])
			metadata, ok := updates[trackID]
			if !ok {
				continue
			}
			genreID, nextGenreMax, err := rawPDBEnsureLookup(&tables, &genreIndex, 1, &genreIDs, genreMax, metadata.Genre)
			if err != nil {
				return nil, err
			}
			genreMax = nextGenreMax
			labelID, nextLabelMax, err := rawPDBEnsureLookup(&tables, &labelIndex, 4, &labelIDs, labelMax, metadata.Label)
			if err != nil {
				return nil, err
			}
			labelMax = nextLabelMax
			next := append([]byte(nil), row...)
			if binary.LittleEndian.Uint32(next[60:]) != genreID {
				binary.LittleEndian.PutUint32(next[60:], genreID)
				changed = true
			}
			if binary.LittleEndian.Uint32(next[40:]) != labelID {
				binary.LittleEndian.PutUint32(next[40:], labelID)
				changed = true
			}
			withComment, commentChanged, err := replacePDBRowString(next, 16, metadata.Comment)
			if err != nil {
				return nil, err
			}
			if commentChanged {
				changed = true
				next = withComment
			}
			tables[i].rows[j] = next
		}
	}
	if !changed {
		return append([]byte(nil), raw...), nil
	}
	return serializeRawPDB(raw[:pageSize], pageSize, tables)
}

func findRawPDBTable(tables []rawPDBTable, typ uint32) int {
	for i := range tables {
		if tables[i].typ == typ {
			return i
		}
	}
	return -1
}

func rawPDBLookup(tables []rawPDBTable, index int) (map[string]uint32, uint32) {
	values := make(map[string]uint32)
	if index < 0 {
		return values, 0
	}
	var maxID uint32
	for _, row := range tables[index].rows {
		if len(row) < 5 {
			continue
		}
		id := binary.LittleEndian.Uint32(row)
		name, err := readDeviceSQLString(row, 4)
		if err != nil {
			continue
		}
		values[name] = id
		if id > maxID {
			maxID = id
		}
	}
	return values, maxID
}

func rawPDBEnsureLookup(tables *[]rawPDBTable, index *int, typ uint32, values *map[string]uint32, maxID uint32, name string) (uint32, uint32, error) {
	if name == "" {
		return 0, maxID, nil
	}
	if id := (*values)[name]; id != 0 {
		return id, maxID, nil
	}
	maxID++
	encoded := EncodeDeviceSQLString(name)
	if len(encoded) > math.MaxUint16 {
		return 0, maxID - 1, fmt.Errorf("pdb: lookup string is too long")
	}
	if *index < 0 {
		*index = len(*tables)
		*tables = append(*tables, rawPDBTable{typ: typ, header: rawPDBHeaderPage(typ), data: rawPDBDataPage(typ)})
	}
	(*tables)[*index].rows = append((*tables)[*index].rows, rawPDBLookupRow(maxID, encoded))
	(*values)[name] = maxID
	return maxID, maxID, nil
}

func rawPDBLookupRow(id uint32, encoded []byte) []byte {
	row := make([]byte, 4, 4+len(encoded))
	binary.LittleEndian.PutUint32(row, id)
	return append(row, encoded...)
}

func replacePDBRowString(row []byte, slot int, value string) ([]byte, bool, error) {
	const offsetBase = 94
	if slot < 0 || slot >= 21 || len(row) < offsetBase+2*(slot+1) {
		return nil, false, fmt.Errorf("pdb: invalid track string slot %d", slot)
	}
	encoded := EncodeDeviceSQLString(value)
	if len(encoded) > math.MaxUint16 {
		return nil, false, fmt.Errorf("pdb: track string is too long")
	}
	ptr := offsetBase + 2*slot
	oldOff := int(binary.LittleEndian.Uint16(row[ptr:]))
	if oldOff == 0 || oldOff >= len(row) {
		if value == "" {
			return row, false, nil
		}
		next := append(append([]byte(nil), row...), encoded...)
		binary.LittleEndian.PutUint16(next[ptr:], uint16(len(row)))
		return next, true, nil
	}
	oldLen, err := pdbStringLength(row, oldOff)
	if err != nil {
		return nil, false, err
	}
	oldBytes := row[oldOff : oldOff+oldLen]
	if bytes.Equal(oldBytes, encoded) {
		return row, false, nil
	}
	delta := len(encoded) - oldLen
	next := make([]byte, len(row)+delta)
	copy(next[:oldOff], row[:oldOff])
	copy(next[oldOff:], encoded)
	copy(next[oldOff+len(encoded):], row[oldOff+oldLen:])
	for i := 0; i < 21; i++ {
		p := offsetBase + 2*i
		off := int(binary.LittleEndian.Uint16(row[p:]))
		switch {
		case off == oldOff:
			// Keep the shared pointer at the replacement string.
		case off > oldOff:
			off += delta
		default:
			continue
		}
		if off < 0 || off > math.MaxUint16 {
			return nil, false, fmt.Errorf("pdb: string offset overflow")
		}
		binary.LittleEndian.PutUint16(next[p:], uint16(off))
	}
	return next, true, nil
}

func pdbStringLength(buf []byte, off int) (int, error) {
	if off < 0 || off >= len(buf) {
		return 0, fmt.Errorf("pdb: string offset outside row")
	}
	kind := buf[off]
	if kind&1 == 1 {
		n := int(kind >> 1)
		if n == 0 || off+n > len(buf) {
			return 0, fmt.Errorf("pdb: invalid short string")
		}
		return n, nil
	}
	if off+4 > len(buf) {
		return 0, fmt.Errorf("pdb: truncated string header")
	}
	n := int(binary.LittleEndian.Uint16(buf[off+1:]))
	if n < 4 || off+n > len(buf) {
		return 0, fmt.Errorf("pdb: invalid string length")
	}
	return n, nil
}

func pdbPageRows(page []byte, pageSize, count int) ([][]byte, error) {
	indexBytes := (count / 16) * 36
	if rem := count % 16; rem != 0 {
		indexBytes += 4 + 2*rem
	}
	heapEnd := pageSize - indexBytes
	if heapEnd < 40 {
		return nil, fmt.Errorf("pdb: row index overlaps header")
	}
	type ref struct{ off int }
	refs := make([]ref, 0, count)
	for i := 0; i < count; i++ {
		base := pageSize - (i/16)*36
		off := 40 + int(binary.LittleEndian.Uint16(page[base-6-2*(i%16):]))
		live := binary.LittleEndian.Uint16(page[base-4:])&(1<<uint(i%16)) != 0
		if !live {
			continue
		}
		if off < 40 || off >= heapEnd {
			return nil, fmt.Errorf("pdb: row outside heap")
		}
		refs = append(refs, ref{off})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].off < refs[j].off })
	rows := make([][]byte, 0, len(refs))
	for i, r := range refs {
		end := heapEnd
		if i+1 < len(refs) {
			end = refs[i+1].off
		}
		if end <= r.off {
			return nil, fmt.Errorf("pdb: overlapping row")
		}
		rows = append(rows, append([]byte(nil), page[r.off:end]...))
	}
	return rows, nil
}

func rawPDBHeaderPage(typ uint32) []byte {
	p := make([]byte, 40)
	p[27] = 0x64
	binary.LittleEndian.PutUint32(p[8:], typ)
	return p
}

func rawPDBDataPage(typ uint32) []byte {
	p := make([]byte, 40)
	p[27] = 0x24
	binary.LittleEndian.PutUint32(p[8:], typ)
	return p
}

func serializeRawPDB(sourceHeader []byte, pageSize int, tables []rawPDBTable) ([]byte, error) {
	if len(tables) > (pageSize-28)/16 {
		return nil, fmt.Errorf("pdb: too many tables")
	}
	pages := [][]byte{make([]byte, pageSize)}
	copy(pages[0], sourceHeader)
	pageRanges := make([][2]uint32, len(tables))
	for ti := range tables {
		t := &tables[ti]
		header := make([]byte, pageSize)
		if len(t.header) > 0 {
			copy(header, t.header)
		} else {
			copy(header, rawPDBHeaderPage(t.typ))
		}
		headerNo := uint32(len(pages))
		binary.LittleEndian.PutUint32(header[4:], headerNo)
		binary.LittleEndian.PutUint32(header[8:], t.typ)
		binary.LittleEndian.PutUint32(header[12:], 0)
		binary.LittleEndian.PutUint32(header[24:], 0)
		pages = append(pages, header)
		last := headerNo
		if len(t.rows) > 0 {
			dataTemplate := t.data
			if len(dataTemplate) == 0 {
				dataTemplate = rawPDBDataPage(t.typ)
			}
			var page []byte
			var pageNo uint32
			rowCount, rowOff := 0, 40
			startPage := func() {
				pageNo = uint32(len(pages))
				page = make([]byte, pageSize)
				copy(page, dataTemplate)
				binary.LittleEndian.PutUint32(page[4:], pageNo)
				binary.LittleEndian.PutUint32(page[8:], t.typ)
				binary.LittleEndian.PutUint32(page[12:], 0)
				page[27] = 0x24
				rowCount, rowOff = 0, 40
				pages = append(pages, page)
			}
			finishPage := func() {
				binary.LittleEndian.PutUint16(page[24:], uint16(rowCount))
				binary.LittleEndian.PutUint16(page[30:], uint16(rowOff-40))
			}
			startPage()
			firstData := pageNo
			for _, row := range t.rows {
				nextCount := rowCount + 1
				indexBytes := (nextCount / 16) * 36
				if rem := nextCount % 16; rem != 0 {
					indexBytes += 4 + 2*rem
				}
				if rowOff == 40 && len(row) > pageSize-indexBytes-40 {
					return nil, fmt.Errorf("pdb: row is too large for a page")
				}
				if rowOff+len(row) > pageSize-indexBytes {
					finishPage()
					nextPage := uint32(len(pages))
					binary.LittleEndian.PutUint32(page[12:], nextPage)
					startPage()
				}
				copy(page[rowOff:], row)
				base := pageSize - (rowCount/16)*36
				binary.LittleEndian.PutUint16(page[base-6-2*(rowCount%16):], uint16(rowOff-40))
				flags := binary.LittleEndian.Uint16(page[base-4:])
				binary.LittleEndian.PutUint16(page[base-4:], flags|(1<<uint(rowCount%16)))
				rowOff += len(row)
				rowCount++
			}
			finishPage()
			last = pageNo
			binary.LittleEndian.PutUint32(header[12:], firstData)
		} else {
			binary.LittleEndian.PutUint32(header[12:], 0)
		}
		pageRanges[ti] = [2]uint32{headerNo, last}
	}

	header := pages[0]
	binary.LittleEndian.PutUint32(header[4:], uint32(pageSize))
	binary.LittleEndian.PutUint32(header[8:], uint32(len(tables)))
	binary.LittleEndian.PutUint32(header[12:], uint32(len(pages)))
	for i, r := range pageRanges {
		off := 28 + i*16
		binary.LittleEndian.PutUint32(header[off:], tables[i].typ)
		binary.LittleEndian.PutUint32(header[off+4:], tables[i].descriptor)
		binary.LittleEndian.PutUint32(header[off+8:], r[0])
		binary.LittleEndian.PutUint32(header[off+12:], r[1])
	}
	out := make([]byte, len(pages)*pageSize)
	for i, page := range pages {
		copy(out[i*pageSize:], page)
	}
	return out, nil
}
