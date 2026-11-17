package prolink

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const nfsReadWindow = 8 // measured on the deck; bounded receive/reorder memory

type nfsRead struct {
	xid, offset, size uint32
	data, packet      []byte
	sent              time.Time
	tries             int
}

// readFile owns one socket and a fixed ring of READs. Replies match by xid;
// only the head is written, so a Range reader never sees a hole in the cache.
func (c *NFSClient) readFile(ctx context.Context, port int, fh []byte, size uint32, w io.Writer, window int) error {
	if size == 0 {
		return nil
	}
	conn, err := c.dial(port)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetReadBuffer(window * nfsChunk * 2)
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = time.Second
	}
	var seed [4]byte
	_, _ = rand.Read(seed[:])
	xid := binary.BigEndian.Uint32(seed[:])
	ring := make([]nfsRead, window)
	pending := make(map[uint32]*nfsRead, window)
	send := func(q *nfsRead) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		if _, err := conn.Write(q.packet); err != nil {
			return err
		}
		q.sent, q.tries = time.Now(), q.tries+1
		return nil
	}
	ask := func(q *nfsRead) error {
		xid++
		q.xid, q.tries = xid, 0
		args := &xdr{}
		args.Write(fh)
		args.u32(q.offset + uint32(len(q.data))).u32(q.size - uint32(len(q.data))).u32(0)
		q.packet = rpcMessage(q.xid, rpcNFS, 2, 6, args.Bytes())
		pending[q.xid] = q
		return send(q)
	}
	next := uint32(0)
	fill := func(q *nfsRead) error {
		q.offset, q.size, q.data = next, min(nfsChunk, size-next), q.data[:0]
		next += q.size
		return ask(q)
	}
	for i := range ring {
		if next < size {
			if err := fill(&ring[i]); err != nil {
				return err
			}
		}
	}
	buf := make([]byte, 64<<10)
	head, done := 0, uint32(0)
	for done < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
			return context.DeadlineExceeded
		}
		for q := &ring[head]; q.size > 0 && uint32(len(q.data)) == q.size; q = &ring[head] {
			n, err := w.Write(q.data)
			if err != nil {
				return err
			}
			if n != len(q.data) {
				return io.ErrShortWrite
			}
			done += q.size
			q.size = 0
			if next < size {
				if err := fill(q); err != nil {
					return err
				}
			}
			head = (head + 1) % window
			if done == size {
				return nil
			}
		}
		deadline := time.Now().Add(timeout)
		for _, q := range pending {
			if !time.Now().Before(q.sent.Add(timeout)) {
				if q.tries == 3 {
					return errors.New("prolink: NFS READ timed out after three attempts")
				}
				if err := send(q); err != nil {
					return err
				}
			}
			if d := q.sent.Add(timeout); d.Before(deadline) {
				deadline = d
			}
		}
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return err
		}
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		r := &reader{b: buf[:n]}
		q := pending[r.u32()]
		if q == nil || r.err != nil {
			continue
		} // stale, duplicate or foreign xid
		if _, err := rpcAccepted(r, rpcNFS, 6); err != nil {
			return err
		}
		if st := r.u32(); r.err != nil || st != 0 {
			return fmt.Errorf("prolink: NFS read error %d", st)
		}
		r.fixed(68)
		data := r.opaque(nfsChunk)
		if r.err != nil {
			return r.err
		}
		if len(data) == 0 {
			return fmt.Errorf("prolink: NFS ended at %d of %d bytes", q.offset+uint32(len(q.data)), size)
		}
		q.data = append(q.data, data[:min(len(data), int(q.size)-len(q.data))]...)
		delete(pending, q.xid)
		if uint32(len(q.data)) < q.size {
			if err := ask(q); err != nil {
				return err
			}
		}
	}
	return nil
}
