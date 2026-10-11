// Command hedgectl trades a Hyperliquid account through an agent wallet and
// runs the Paper/Hyperliquid hedge. Every write is a dry run (signed, never
// posted) unless -live is passed.
//
// Environment:
//
//	HL_NETWORK            mainnet | testnet (required, no default)
//	HL_ACCOUNT_ADDRESS    master wallet the agent trades for
//	HL_AGENT_PRIVATE_KEY  agent (API) wallet key; never the master key
//	HL_BUILDER_ADDRESS    optional builder; HL_BUILDER_FEE in tenths of a bp
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hedge"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hyperliquid"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

const usage = `usage:
  hedgectl hl quote    -coin BTC
  hedgectl hl position -coin BTC
  hedgectl hl order    -coin BTC -side buy|sell -size 0.001 [-price P] [-tif Ioc|Gtc|Alo] [-reduce-only] [-slippage 0.01] [-live]
  hedgectl hl cancel   -coin BTC -oid 123 [-live]
  hedgectl hl leverage -coin BTC -leverage 10 [-isolated] [-live]
  hedgectl hedge open  -coin BTC -mint-side buy|sell -size 0.001 [-paper-leverage 100] [-live]
  hedgectl hedge close -coin BTC [-live]`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New(usage)
	}
	group, cmd, rest := args[0], args[1], args[2:]
	fs := flag.NewFlagSet(group+" "+cmd, flag.ContinueOnError)
	coin := fs.String("coin", "BTC", "coin / symbol")
	live := fs.Bool("live", false, "post signed actions (default: dry run)")
	side := fs.String("side", "", "buy or sell")
	mintSide := fs.String("mint-side", "", "Paper leg side; the Hyperliquid leg takes the opposite")
	size := fs.String("size", "", "base size")
	price := fs.String("price", "", "limit price; empty for market")
	tif := fs.String("tif", "Gtc", "time in force for limit orders")
	reduceOnly := fs.Bool("reduce-only", false, "reduce-only")
	slippage := fs.String("slippage", "0.01", "market order slippage fraction")
	oid := fs.String("oid", "", "order id")
	leverage := fs.Int("leverage", 0, "leverage")
	isolated := fs.Bool("isolated", false, "isolated margin")
	paperLeverage := fs.Int("paper-leverage", 100, "Paper leverage (1..1000)")
	maxSpread := fs.String("max-spread-bps", "2", "refuse to open above this Hyperliquid spread")
	bookShare := fs.String("max-book-share", "0.5", "max share of Hyperliquid top-of-book size")
	closeTimeout := fs.Duration("close-timeout", 5*time.Minute, "max wait for Paper's queued close")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	limits := map[string]decimal.Decimal{}
	for name, raw := range map[string]string{"slippage": *slippage, "max-spread-bps": *maxSpread, "max-book-share": *bookShare} {
		v, err := decimal.NewFromString(raw)
		if err != nil {
			return fmt.Errorf("-%s: %w", name, err)
		}
		limits[name] = v
	}
	hl, err := newHyperliquid(!*live, limits["slippage"])
	if err != nil {
		return err
	}
	switch group + " " + cmd {
	case "hl quote":
		return printJSON(hl.Quote(ctx, *coin))
	case "hl position":
		return printJSON(hl.Position(ctx, *coin))
	case "hl order":
		req := venue.OrderRequest{Symbol: *coin, Side: venue.Side(*side), TimeInForce: venue.TimeInForce(*tif), ReduceOnly: *reduceOnly}
		if req.Side != venue.Buy && req.Side != venue.Sell {
			return errors.New("-side must be buy or sell")
		}
		if req.Size, err = decimal.NewFromString(*size); err != nil {
			return fmt.Errorf("-size: %w", err)
		}
		if *price != "" {
			if req.LimitPrice, err = decimal.NewFromString(*price); err != nil {
				return fmt.Errorf("-price: %w", err)
			}
		}
		return printJSON(hl.PlaceOrder(ctx, req))
	case "hl cancel":
		return hl.CancelOrder(ctx, *coin, *oid)
	case "hl leverage":
		return hl.SetLeverage(ctx, *coin, *leverage, !*isolated)
	case "hedge open", "hedge close":
		// The Paper gateway is reserved; until it is wired the engine refuses
		// before sending any Hyperliquid order.
		pv, err := paper.NewVenue(paper.Config{Leverage: *paperLeverage}, nil)
		if err != nil {
			return err
		}
		engine, err := hedge.New(hedge.Config{
			Mint: hedge.Leg{Venue: pv, Symbol: *coin}, Hedge: hedge.Leg{Venue: hl, Symbol: *coin},
			MaxSpreadBps: limits["max-spread-bps"], MaxBookShare: limits["max-book-share"],
			Slippage: limits["slippage"], CloseTimeout: *closeTimeout, PollInterval: 2 * time.Second,
		})
		if err != nil {
			return err
		}
		if cmd == "close" {
			return printJSON(engine.Close(ctx))
		}
		sz, err := decimal.NewFromString(*size)
		if err != nil {
			return fmt.Errorf("-size: %w", err)
		}
		if s := venue.Side(*mintSide); s != venue.Buy && s != venue.Sell {
			return errors.New("-mint-side must be buy or sell")
		}
		return printJSON(engine.Open(ctx, venue.Side(*mintSide), sz))
	}
	return errors.New(usage)
}

func newHyperliquid(dryRun bool, slippage decimal.Decimal) (*hyperliquid.Venue, error) {
	network, err := hyperliquid.ParseNetwork(os.Getenv("HL_NETWORK"))
	if err != nil {
		return nil, err
	}
	signer, err := evmsign.NewLocalSigner(os.Getenv("HL_AGENT_PRIVATE_KEY"))
	if err != nil {
		return nil, fmt.Errorf("HL_AGENT_PRIVATE_KEY: %w", err)
	}
	cfg := hyperliquid.VenueConfig{AccountAddress: os.Getenv("HL_ACCOUNT_ADDRESS"), DefaultSlippage: slippage}
	if b := os.Getenv("HL_BUILDER_ADDRESS"); b != "" {
		fee, err := strconv.Atoi(os.Getenv("HL_BUILDER_FEE"))
		if err != nil || fee <= 0 {
			return nil, errors.New("HL_BUILDER_FEE must be a positive integer (tenths of a bp) when HL_BUILDER_ADDRESS is set")
		}
		cfg.Builder = &hyperliquid.BuilderWire{B: b, F: fee}
	}
	exchange := hyperliquid.NewExchangeClient(network, signer, nil)
	exchange.DryRun = dryRun
	return hyperliquid.NewVenue(cfg, hyperliquid.NewInfoClient(network, nil), exchange)
}

func printJSON[T any](v T, err error) error {
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
