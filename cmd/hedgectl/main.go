// Command hedgectl trades Hyperliquid through an agent wallet and farms
// Papertrade PAPER with same-platform long+short pairs. Every write is a dry
// run (signed, never posted) unless -live is passed.
//
// Environment:
//
//	HL_NETWORK                 mainnet | testnet (hl commands)
//	HL_ACCOUNT_ADDRESS         master wallet the agent trades for
//	HL_AGENT_PRIVATE_KEY       agent (API) wallet key; never the master key
//	HL_BUILDER_ADDRESS         optional builder; HL_BUILDER_FEE in tenths of a bp
//	PAPER_ACCOUNT_ADDRESS      Papertrade master wallet (paper/farm commands)
//	PAPER_SESSION_PRIVATE_KEY  registered Paper session key; it can trade and
//	                           stake but not withdraw. Never the master key.
//	PAPER_RELAYER_URL          optional, default https://exchange.papertrade.xyz
//	PAPER_BUILD                optional X-Papertrade-Build override
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

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hyperliquid"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

const usage = `usage:
  hedgectl hl quote    -coin BTC
  hedgectl hl position -coin BTC
  hedgectl hl order    -coin BTC -side buy|sell -size 0.001 [-price P] [-tif Ioc|Gtc|Alo] [-reduce-only] [-slippage 0.01] [-live]
  hedgectl hl cancel   -coin BTC -oid 123 [-live]
  hedgectl hl leverage -coin BTC -leverage 10 [-isolated] [-live]

  hedgectl paper status
  hedgectl paper econ          -coin BTC -notional 10000 [-moves 0.0003,0.001,0.005] [-mint-rate R]
  hedgectl paper account
  hedgectl paper trades        [-limit 20]
  hedgectl paper session-new
  hedgectl paper register-data -session 0xKEY [-days 30]
  hedgectl paper register      -data typed.json -signature 0xSIG [-live]
  hedgectl paper register-web  [-listen 127.0.0.1:8547] [-days 30] [-live]   # sign in a browser wallet
  hedgectl paper open          -coin BTC -side long|short -margin 10 -leverage 1000 [-live]
  hedgectl paper close         -ids 1,2 [-live]
  hedgectl paper stake         -amount 100 | -all [-live]
  hedgectl paper claim         [-live]

  hedgectl web [-listen 127.0.0.1:8547] [-live]   # local console: volatility, recommendations, runs

  hedgectl farm run -coin BTC -margin 10 -leverage 1000 -close-move 0.0003 -bust-guard 0.0001 [-cycles 1] [-max-hold 0] [-stake] [-live]`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) >= 1 && args[0] == "web" {
		return runWeb(ctx, args[1:])
	}
	if len(args) < 2 {
		return errors.New(usage)
	}
	switch args[0] {
	case "hl":
		return runHyperliquid(ctx, args[1], args[2:])
	case "paper":
		return runPaper(ctx, args[1], args[2:])
	case "farm":
		return runFarm(ctx, args[1], args[2:])
	}
	return errors.New(usage)
}

func runHyperliquid(ctx context.Context, cmd string, rest []string) error {
	fs := flag.NewFlagSet("hl "+cmd, flag.ContinueOnError)
	coin := fs.String("coin", "BTC", "coin")
	live := fs.Bool("live", false, "post signed actions (default: dry run)")
	side := fs.String("side", "", "buy or sell")
	size := fs.String("size", "", "base size")
	price := fs.String("price", "", "limit price; empty for market")
	tif := fs.String("tif", "Gtc", "time in force for limit orders")
	reduceOnly := fs.Bool("reduce-only", false, "reduce-only")
	slippage := fs.String("slippage", "0.01", "market order slippage fraction")
	oid := fs.String("oid", "", "order id")
	leverage := fs.Int("leverage", 0, "leverage")
	isolated := fs.Bool("isolated", false, "isolated margin")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	slip, err := decimal.NewFromString(*slippage)
	if err != nil {
		return fmt.Errorf("-slippage: %w", err)
	}
	hl, err := newHyperliquid(!*live, slip)
	if err != nil {
		return err
	}
	switch cmd {
	case "quote":
		return printJSON(hl.Quote(ctx, *coin))
	case "position":
		return printJSON(hl.Position(ctx, *coin))
	case "order":
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
	case "cancel":
		return hl.CancelOrder(ctx, *coin, *oid)
	case "leverage":
		return hl.SetLeverage(ctx, *coin, *leverage, !*isolated)
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
