// Command stems-processor stands in for Engine 5.0.4's native Stems child
// process on SC5000. It forwards the child's stdin/stdout byte stream to a
// remote renderer, by default through the cloud relay the deck already uses
// for logins (see internal/stems.ResolveClient for the override variables).
// With no relay set up, or one it cannot reach, it looks for sc5000-stemsd
// on a computer in the same LAN (stems.FindLAN); failing that the deck
// splits stems itself (stems.LocalRender).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/stems"
	"github.com/Cd1s/sc5000-custom-os/pkg/resourceguard"
)

func main() {
	// Engine supplies -s -hv 1.0.0 -pv <parent version>. No secrets in argv.
	client, err := stems.ResolveClient(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stems backend:", err)
		os.Exit(1)
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
		os.Exit(1)
	}
}

// run tries the configured renderer, then a LAN renderer, then local. It only
// moves on when the previous one could not even be dialled, so Engine's
// stream is never split between two renderers.
func run(ctx context.Context, client stems.Client, findLAN func(context.Context) (string, bool), local func(io.Reader, io.Writer) error, in io.Reader, out io.Writer) error {
	if client.Endpoint != "" {
		err := stems.Proxy(ctx, client.Endpoint, client.Secret, in, out)
		if !errors.Is(err, stems.ErrUnreachable) {
			if err != nil {
				return fmt.Errorf("remote stems (%s): %w", client.Source, err)
			}
			return nil
		}
		fmt.Fprintf(os.Stderr, "remote stems (%s): %v; trying the LAN\n", client.Source, err)
	}
	if endpoint, ok := findLAN(ctx); ok {
		err := stems.Proxy(ctx, endpoint, "", in, out)
		if !errors.Is(err, stems.ErrUnreachable) {
			if err != nil {
				return fmt.Errorf("LAN stems (%s): %w", endpoint, err)
			}
			return nil
		}
	}
	if err := local(in, out); err != nil {
		return fmt.Errorf("local stems: %w", err)
	}
	return nil
}
