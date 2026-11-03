// Command stems-processor stands in for Engine 5.0.4's native Stems child
// process on SC5000. It forwards the child's stdin/stdout byte stream to a
// remote renderer, by default through the cloud relay the deck already uses
// for logins (see internal/stems.ResolveClient for the override variables).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Cd1s/sc5000-custom-os/internal/stems"
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
	if err = stems.Proxy(ctx, client.Endpoint, client.Secret, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "remote stems (%s): %v\n", client.Source, err)
		os.Exit(1)
	}
}
