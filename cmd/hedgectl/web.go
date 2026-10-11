package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/console"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hyperliquid"
)

// runWeb serves the local console until ctx ends. Stopping leaves any open
// pair in place (the run logs its ids) rather than closing on a signal.
func runWeb(ctx context.Context, rest []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8547", "loopback address")
	live := fs.Bool("live", false, "post signed intents (default: dry run)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("-listen must be a loopback address")
	}
	srv, err := console.New(paperClient(!*live), hyperliquid.NewInfoClient(hyperliquid.Mainnet, nil), *live)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	mode := "DRY-RUN (writes are signed, never posted)"
	if *live {
		mode = "LIVE (writes are posted)"
	}
	fmt.Fprintf(os.Stderr, "Paper console on http://%s  %s\nCtrl-C stops runs (open pairs stay open) and exits.\n", ln.Addr(), mode)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	srv.Stop(false)
	for sym, ids := range srv.OpenPairs() {
		fmt.Fprintf(os.Stderr, "%s pair %v left open; close it from the console or `hedgectl paper close -ids`\n", sym, ids)
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(shut)
}
