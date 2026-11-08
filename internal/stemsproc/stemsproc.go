// Package stemsproc is stems-processor. It stands in for Engine 5.0.4's native Stems child
// process on SC5000. It forwards the child's stdin/stdout byte stream to a
// remote renderer, by default through the cloud relay the deck already uses
// for logins (see internal/stems.ResolveClient for the override variables).
// With no relay set up, or one it cannot reach, it looks for sc5000-stemsd
// on a computer in the same LAN (stems.FindLAN); failing that the deck
// splits stems itself (stems.LocalRender). sc5000-suite runs Main.
package stemsproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

// Main is stems-processor. Engine passes -s -hv 1.0.0 -pv <version>; none is
// needed, and no secret is ever taken from argv.
func Main(args []string) int {
	_ = args
	client, err := stems.ResolveClient(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stems backend:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	findLAN := func(ctx context.Context) (string, bool) {
		return stems.FindLAN(ctx, stems.DefaultLANState, 350*time.Millisecond)
	}
	local := func(in io.Reader, out io.Writer) error {
		// Split on the deck, never at the audio's expense.
		if _, err := resourceguard.New(resourceguard.DefaultConfig()).ApplyToProcess(0); err != nil {
			fmt.Fprintln(os.Stderr, "local stems: resource guard:", err)
		}
		return stems.LocalRender(in, out)
	}
	if err = run(ctx, client, findLAN, local, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// run tries the configured renderer, then a LAN renderer, then local. It moves
// on only while the next renderer can still be given the whole stream: the
// renderer could not be dialled, or answered 502/503/504 having read no more than
// the replayable prefix (the greeting header, #41). So the stream is never split
// between two renderers.
func run(ctx context.Context, client stems.Client, findLAN func(context.Context) (string, bool), local func(io.Reader, io.Writer) error, in io.Reader, out io.Writer) error {
	src := newStdinMux(in)
	next := func(err error, v *view) bool {
		v.abandon()
		return errors.Is(err, stems.ErrUnreachable) || (errors.Is(err, stems.ErrUnavailable) && v.replayable())
	}
	if client.Endpoint != "" {
		v := src.view()
		err := stems.Proxy(ctx, client.Endpoint, client.Secret, v, out)
		if !next(err, v) {
			if err != nil {
				return fmt.Errorf("remote stems (%s): %w", client.Source, err)
			}
			return nil
		}
		fmt.Fprintf(os.Stderr, "remote stems (%s): %v; trying the LAN\n", client.Source, err)
	}
	if endpoint, ok := findLAN(ctx); ok {
		v := src.view()
		err := stems.Proxy(ctx, endpoint, "", v, out)
		if !next(err, v) {
			if err != nil {
				return fmt.Errorf("LAN stems (%s): %w", endpoint, err)
			}
			return nil
		}
		fmt.Fprintf(os.Stderr, "LAN stems (%s): %v; rendering locally\n", endpoint, err)
	}
	if err := local(src.view(), out); err != nil {
		return fmt.Errorf("local stems: %w", err)
	}
	return nil
}

// stdinMux hands Engine's stdin to one renderer at a time. A renderer that is
// given up may still have a read in flight (its HTTP upload); whatever that
// read brings in is kept for the next renderer instead of being lost. The first
// replayLimit bytes are retained and replayed to every new view; once a view
// reads past them the stream is committed and consumed bytes are dropped.
type stdinMux struct {
	mu        sync.Mutex
	cond      *sync.Cond
	src       io.Reader
	buf       []byte
	base      int64 // stream offset of buf[0]
	committed bool
	err       error
	reading   bool
}

func newStdinMux(src io.Reader) *stdinMux {
	m := &stdinMux{src: src}
	m.cond = sync.NewCond(&m.mu)
	return m
}

func (m *stdinMux) fill() {
	b := make([]byte, 64<<10)
	n, err := m.src.Read(b)
	m.mu.Lock()
	m.buf = append(m.buf, b[:n]...)
	if err != nil {
		m.err = err
	}
	m.reading = false
	m.cond.Broadcast()
	m.mu.Unlock()
}

// replayLimit covers Engine's greeting header with room for the first block.
const replayLimit = 1 << 20

func (m *stdinMux) view() *view { return &view{m: m} }

var errAbandoned = errors.New("stems: renderer given up")

type view struct {
	m    *stdinMux
	dead bool  // guarded by m.mu
	pos  int64 // stream offset of the next byte for this renderer, guarded by m.mu
}

func (v *view) Read(p []byte) (int, error) {
	m := v.m
	m.mu.Lock()
	defer m.mu.Unlock()
	for v.pos >= m.base+int64(len(m.buf)) && m.err == nil && !v.dead {
		if !m.reading {
			m.reading = true
			go m.fill()
		}
		m.cond.Wait()
	}
	if v.dead {
		return 0, errAbandoned
	}
	if v.pos < m.base {
		return 0, errAbandoned
	}
	if v.pos < m.base+int64(len(m.buf)) {
		n := copy(p, m.buf[v.pos-m.base:])
		v.pos += int64(n)
		if v.pos > replayLimit {
			m.committed = true
		}
		if m.committed { // nobody can restart the stream now
			m.buf = m.buf[v.pos-m.base:]
			m.base = v.pos
		}
		return n, nil
	}
	return 0, m.err
}

func (v *view) abandon() {
	v.m.mu.Lock()
	v.dead = true
	v.m.cond.Broadcast()
	v.m.mu.Unlock()
}

// replayable reports whether a fresh view would still see the stream from byte 0.
func (v *view) replayable() bool {
	v.m.mu.Lock()
	defer v.m.mu.Unlock()
	return !v.m.committed
}
