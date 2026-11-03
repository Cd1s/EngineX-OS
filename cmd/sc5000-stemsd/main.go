// Command sc5000-stemsd runs the Engine 5.0.4 Stems worker for remote decks.
// It listens on loopback by default; sc5000-relay is the public entry point
// and forwards authenticated /v1/engine sessions here.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cd1s/sc5000-custom-os/internal/stems"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:6596", "listen address")
	token := flag.String("token-file", stems.DefaultTokenFile, "internal credential file")
	worker := flag.String("worker", "/opt/sc5000-stems/worker.py", "protocol worker")
	flag.Parse()
	s, e := stems.Secret(*token)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	b := &stems.Backend{Secret: s, Worker: []string{"/usr/bin/python3", "-u", *worker}, Slot: make(chan struct{}, 1), Log: os.Stderr}
	server := &http.Server{Addr: *listen, Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server.BaseContext = func(net.Listener) context.Context { return ctx }
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(c); err != nil {
			server.Close()
		}
		close(shutdownDone)
	}()
	fmt.Fprintln(os.Stderr, "sc5000-stemsd listening on", *listen)
	if e = server.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
}
