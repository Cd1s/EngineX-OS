package prolink

// dbserver client for a computer running rekordbox 7 with LINK on: browse its
// collection and ask where a track's audio file is. Wire shapes follow what
// rekordbox answered to cmd/rbx-probe: one sequential session per asking player,
// its own collection slot 4, track type 1, a 0x0100 teardown at the end.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	rekordboxQueryPort = 12523
	rekordboxPlayer    = 5 // an asking player number rekordbox accepts (1, 4, 5 and 7 do, 41 does not)
	rbxCollection      = 4
	rbxTimeout         = 5 * time.Second
	rbxChunk           = 64
	rbxMaxRows         = 1 << 16
)

// Menu row kinds of a rekordbox collection.
const (
	RekordboxFolder   = 1
	RekordboxPlaylist = 8
	rbxPath           = 0
)

// RekordboxItem is one menu row: a track, a playlist or a folder. For the audio path
// row of a track, Parent is the file's size in bytes.
type RekordboxItem struct {
	Parent, ID, Kind uint32
	Label            string
}

// RekordboxLink is one dbserver session with a computer running rekordbox. Not for
// concurrent use: the computer serves one session per asking player at a time.
type RekordboxLink struct {
	c    net.Conn
	r    *bufio.Reader
	tx   uint32
	stop func() bool
}

// DialRekordbox opens a session with the rekordbox computer at ip. Close it when done.
func DialRekordbox(ctx context.Context, ip net.IP) (*RekordboxLink, error) {
	return dialRekordbox(ctx, ip, rekordboxQueryPort)
}

func dialRekordbox(ctx context.Context, ip net.IP, queryPort int) (*RekordboxLink, error) {
	var d net.Dialer
	q, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(queryPort)))
	if err != nil {
		return nil, err
	}
	_ = q.SetDeadline(time.Now().Add(rbxTimeout))
	var port [2]byte
	_, err = q.Write(append([]byte{0, 0, 0, 15}, "RemoteDBServer\x00"...))
	if err == nil {
		_, err = io.ReadFull(q, port[:])
	}
	q.Close()
	if err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(port[:])
	if n == 0 || n == 0xffff {
		return nil, errors.New("prolink: rekordbox does not serve its library (LINK is off)")
	}
	c, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(int(n))))
	if err != nil {
		return nil, err
	}
	l := &RekordboxLink{c: c, r: bufio.NewReader(c), stop: context.AfterFunc(ctx, func() { c.Close() })}
	hello := dbNumber(1).appendTo(nil)
	_ = c.SetDeadline(time.Now().Add(rbxTimeout))
	got := make([]byte, len(hello))
	if _, err = c.Write(hello); err == nil {
		_, err = io.ReadFull(l.r, got)
	}
	if err == nil && !bytes.Equal(got, hello) {
		err = errors.New("prolink: rekordbox did not greet back")
	}
	if err == nil {
		var ack dbMessage
		if ack, err = l.exchange(dbMessage{0xfffffffe, 0, []dbField{dbNumber(rekordboxPlayer)}}); err == nil && ack.kind != 0x4000 {
			err = fmt.Errorf("prolink: rekordbox refused the session (0x%04x)", ack.kind)
		}
	}
	if err != nil {
		l.stop()
		c.Close()
		return nil, err
	}
	return l, nil
}

// Close ends the session so the computer frees the asking player number at once.
func (l *RekordboxLink) Close() {
	l.stop()
	_ = l.c.SetDeadline(time.Now().Add(250 * time.Millisecond))
	if _, err := l.c.Write((dbMessage{0xfffffffe, 0x0100, nil}).bytes()); err == nil {
		_ = l.c.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
		_, _ = l.c.Read(make([]byte, 1))
	}
	l.c.Close()
}

func (l *RekordboxLink) exchange(m dbMessage) (dbMessage, error) {
	_ = l.c.SetDeadline(time.Now().Add(rbxTimeout))
	if _, err := l.c.Write(m.bytes()); err != nil {
		return dbMessage{}, err
	}
	return readDBMessage(l.r)
}

func (l *RekordboxLink) call(kind uint16, args ...dbField) (dbMessage, error) {
	l.tx++
	return l.exchange(dbMessage{l.tx, kind, args})
}

// menu runs a request on the collection's main menu and reads every row it lists.
func (l *RekordboxLink) menu(kind uint16, args ...uint32) ([]RekordboxItem, error) {
	// asking player, menu 1, slot 4, track type 1 (rekordbox)
	where := uint32(rekordboxPlayer)<<24 | 1<<16 | rbxCollection<<8 | 1
	fields := []dbField{dbNumber(where)}
	for _, a := range args {
		fields = append(fields, dbNumber(a))
	}
	r, err := l.call(kind, fields...)
	if err != nil {
		return nil, err
	}
	if r.kind != 0x4000 || len(r.args) < 2 {
		return nil, fmt.Errorf("prolink: rekordbox answered 0x%04x to 0x%04x", r.kind, kind)
	}
	count := r.args[1].n
	if count == 0xffffffff { // no results
		count = 0
	}
	count = min(count, rbxMaxRows)
	var items []RekordboxItem
	for off := uint32(0); off < count; off += rbxChunk {
		n := min(count-off, rbxChunk)
		h, err := l.call(0x3000, dbNumber(where), dbNumber(off), dbNumber(n), dbNumber(0), dbNumber(count), dbNumber(0))
		if err != nil {
			return items, err
		}
		if h.kind != 0x4001 {
			return items, fmt.Errorf("prolink: rekordbox answered 0x%04x to a menu render", h.kind)
		}
		for i := uint32(0); ; i++ {
			_ = l.c.SetReadDeadline(time.Now().Add(rbxTimeout))
			m, err := readDBMessage(l.r)
			if err != nil {
				return items, err
			}
			if m.kind == 0x4201 {
				break
			}
			if i == n || m.kind != 0x4101 || len(m.args) < 7 || m.args[3].tag != 0x26 {
				return items, errors.New("prolink: malformed rekordbox menu row")
			}
			label := strings.TrimSuffix(strings.TrimPrefix(m.args[3].text(), "\ufffa"), "\ufffb")
			items = append(items, RekordboxItem{Parent: m.args[0].n, ID: m.args[1].n, Kind: m.args[6].n, Label: label})
		}
	}
	return items, nil
}

// Tracks lists every track of the collection.
func (l *RekordboxLink) Tracks() ([]RekordboxItem, error) { return l.menu(0x1004, 0) }

// Playlists lists the folders and playlists inside folder (0 is the top).
func (l *RekordboxLink) Playlists(folder uint32) ([]RekordboxItem, error) {
	return l.menu(0x1105, 0, folder, 1)
}

// PlaylistTracks lists the tracks of a playlist, in its order.
func (l *RekordboxLink) PlaylistTracks(id uint32) ([]RekordboxItem, error) {
	return l.menu(0x1105, 0, id, 0)
}

// Path asks where a track's audio file is on the computer, and its size in bytes.
func (l *RekordboxLink) Path(id uint32) (string, uint32, error) {
	rows, err := l.menu(0x2102, id)
	for _, r := range rows {
		if r.Kind&0xffff == rbxPath && r.Label != "" {
			return r.Label, r.Parent, nil
		}
	}
	if err == nil {
		err = errors.New("prolink: rekordbox gave no file path for the track")
	}
	return "", 0, err
}
