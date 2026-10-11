package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/farm"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hyperliquid"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
)

func runFarm(ctx context.Context, cmd string, rest []string) error {
	if cmd != "run" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("farm run", flag.ContinueOnError)
	coin := fs.String("coin", "BTC", "BTC or ETH")
	margin := fs.String("margin", "10", "margin per leg, USD")
	leverage := fs.Int("leverage", 1000, "leverage per leg")
	closeMove := fs.String("close-move", "0.0003", "close both legs once either leg is this far adverse")
	bustGuard := fs.String("bust-guard", "0.0001", "close early when a leg is this close to its bust price")
	maxHold := fs.Duration("max-hold", 0, "close after this long regardless (0: no limit)")
	cycles := fs.Int("cycles", 1, "pairs to run back to back")
	stake := fs.Bool("stake", false, "stake the unstaked PAPER balance after the last cycle")
	intentTTL := fs.Duration("intent-ttl", paper.DefaultIntentTTL, "intent deadline; bounds how long a stale intent can still execute")
	poll := fs.Duration("poll", time.Second, "price and position poll interval")
	live := fs.Bool("live", false, "post intents (default: plan only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *cycles < 1 {
		return errors.New("-cycles must be at least 1")
	}

	client := paperClient(true)
	ts, err := client.Trading(ctx)
	if err != nil {
		return err
	}
	market, err := ts.Market(*coin)
	if err != nil {
		return err
	}
	summary, err := client.Summary(ctx)
	if err != nil {
		return err
	}
	cfg := farm.Config{
		Symbol:       *coin,
		Leverage:     *leverage,
		MaxHold:      *maxHold,
		FillTimeout:  30 * time.Second,
		CloseTimeout: 30 * time.Second,
		CloseRetries: 3,
		PollInterval: *poll,
		MinMargin:    ts.MinimumMargin,
		MinNotional:  ts.MinimumNotional,
	}
	for _, f := range []struct {
		name, src string
		dst       *decimal.Decimal
	}{{"-margin", *margin, &cfg.Margin}, {"-close-move", *closeMove, &cfg.CloseMove}, {"-bust-guard", *bustGuard, &cfg.BustGuard}} {
		if *f.dst, err = decimal.NewFromString(f.src); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	if *leverage > market.MaxLeverage {
		return fmt.Errorf("-leverage %d above %s max %d", *leverage, *coin, market.MaxLeverage)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	notional := cfg.Margin.Mul(decimal.NewFromInt(int64(cfg.Leverage)))
	estimate := paper.EstimatePair(market.Impact, notional.InexactFloat64(), cfg.CloseMove.InexactFloat64(),
		summary.MintRate(), summary.QueueBalance.IsZero())
	acct, err := paperAccount(!*live, *intentTTL)
	if err != nil {
		return err
	}

	if !*live {
		in, _ := paper.InstrumentBySymbol(*coin)
		long, err := acct.Intents.Open(in, true, paper.ToRaw(cfg.Margin), cfg.Leverage)
		if err != nil {
			return err
		}
		short, err := acct.Intents.Open(in, false, paper.ToRaw(cfg.Margin), cfg.Leverage)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Plan only: nothing was posted. Pass -live to farm.")
		return printJSON(map[string]any{
			"paused":       ts.Paused,
			"notional":     notional,
			"bustDistance": paper.MaxSafeMove(cfg.Leverage) + paper.BustBuffer,
			"mintRate":     summary.MintRate(),
			"perCycle":     estimate,
			"cycles":       *cycles,
			"openIntents":  []paper.Intent{long, short},
		}, nil)
	}

	if ts.Paused {
		return errors.New("paper trading is paused")
	}
	w, err := acct.Client.Wallet(ctx, acct.Intents.User())
	if err != nil {
		return err
	}
	if !w.HasSessionKey(acct.Intents.Signer(), time.Now().Add(time.Hour)) {
		return errors.New("PAPER_SESSION_PRIVATE_KEY is not a registered, unexpired session key for PAPER_ACCOUNT_ADDRESS")
	}
	available, err := paper.FromRaw(w.Funds.AvailableBalance)
	if err != nil {
		return err
	}
	if need := cfg.Margin.Mul(decimal.NewFromInt(2)); available.LessThan(need) {
		return fmt.Errorf("available balance %s below the %s two legs need", available, need)
	}

	info := hyperliquid.NewInfoClient(hyperliquid.Mainnet, nil)
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", args...)
	}
	farmer, err := farm.New(cfg, acct, hyperliquidMid(info), farm.WithLogger(logf))
	if err != nil {
		return err
	}
	var results []farm.Result
	for i := 0; i < *cycles; i++ {
		logf("cycle %d/%d: expected cost %.4f for %.0f PAPER", i+1, *cycles, estimate.NetCost, estimate.PaperMinted)
		res, err := farmer.RunCycle(ctx)
		if err != nil {
			printJSON(results, nil)
			return fmt.Errorf("cycle %d: %w", i+1, err)
		}
		results = append(results, res)
	}
	if *stake {
		w, err := acct.Client.Wallet(ctx, acct.Intents.User())
		if err != nil {
			return err
		}
		if amt, ok := new(big.Int).SetString(w.Paper.Balance, 10); ok && amt.Sign() > 0 {
			id, err := acct.Stake(ctx, amt)
			if err != nil {
				return fmt.Errorf("stake: %w", err)
			}
			logf("staked %s PAPER (intent %s)", w.Paper.Balance, id)
		}
	}
	return printJSON(results, nil)
}

// hyperliquidMid tracks the Hyperliquid BBO mid, the price Paper settles at.
func hyperliquidMid(info *hyperliquid.InfoClient) farm.PriceFunc {
	return func(ctx context.Context, symbol string) (decimal.Decimal, error) {
		b, err := info.L2Book(ctx, symbol)
		if err != nil {
			return decimal.Zero, err
		}
		if len(b.Levels[0]) == 0 || len(b.Levels[1]) == 0 {
			return decimal.Zero, fmt.Errorf("hyperliquid %s book is one-sided", symbol)
		}
		return b.Levels[0][0].Px.Add(b.Levels[1][0].Px).Div(decimal.NewFromInt(2)), nil
	}
}
