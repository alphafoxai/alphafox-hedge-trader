// Package hedge runs the delta-neutral loss-mining loop: a "mint" leg on the
// venue that rewards losses (Paper) and an opposite "hedge" leg on a liquid
// venue (Hyperliquid), so net PnL is roughly fees.
//
// Sequencing is the whole point of this package:
//   - Open: hedge leg first, sized by what actually filled, then the mint leg.
//     If the mint leg fails the hedge leg is unwound, so we never hold a
//     naked hedge on purpose.
//   - Close: mint leg first, then wait until the mint venue reports flat
//     (Paper queues closes), and only then close the hedge leg. Closing the
//     hedge early would leave the mint leg unhedged and liquidatable.
package hedge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

type Leg struct {
	Venue  venue.Venue
	Symbol string
}

type Config struct {
	Mint  Leg
	Hedge Leg
	// MaxSpreadBps rejects an open when the hedge venue's spread is wider.
	MaxSpreadBps decimal.Decimal
	// MaxBookShare caps size at this fraction of the hedge venue's top level,
	// so the hedge leg does not walk the book (e.g. 0.5).
	MaxBookShare decimal.Decimal
	// Slippage bounds the hedge leg's market orders.
	Slippage decimal.Decimal
	// CloseTimeout bounds how long Close waits for the mint leg to go flat.
	CloseTimeout time.Duration
	PollInterval time.Duration
}

func (c Config) validate() error {
	switch {
	case c.Mint.Venue == nil || c.Hedge.Venue == nil:
		return errors.New("hedge needs both venues")
	case c.Mint.Symbol == "" || c.Hedge.Symbol == "":
		return errors.New("hedge needs both symbols")
	case !c.MaxSpreadBps.IsPositive(), !c.MaxBookShare.IsPositive(), !c.Slippage.IsPositive():
		return errors.New("spread, book share and slippage limits must be positive")
	case c.CloseTimeout <= 0 || c.PollInterval <= 0:
		return errors.New("close timeout and poll interval must be positive")
	}
	return nil
}

type Engine struct {
	cfg   Config
	sleep func(context.Context, time.Duration) error
}

func New(cfg Config) (*Engine, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Engine{cfg: cfg, sleep: sleepCtx}, nil
}

type OpenResult struct {
	MintSide venue.Side
	Hedge    venue.OrderResult
	Mint     venue.OrderResult
	Quote    venue.Quote
}

// ErrUnhedged means a leg is exposed without its counterpart and needs
// operator attention.
var ErrUnhedged = errors.New("position left unhedged")

// Open puts on size on the mint venue at mintSide, hedged by the opposite
// side on the hedge venue.
func (e *Engine) Open(ctx context.Context, mintSide venue.Side, size decimal.Decimal) (OpenResult, error) {
	res := OpenResult{MintSide: mintSide}
	if !size.IsPositive() {
		return res, errors.New("size must be positive")
	}
	hedgeSide := mintSide.Opposite()
	// Probe the mint venue before any order: an unwired or down mint venue
	// would otherwise cost a hedge round trip in fees.
	if _, err := e.cfg.Mint.Venue.Quote(ctx, e.cfg.Mint.Symbol); err != nil {
		return res, fmt.Errorf("mint venue not ready: %w", err)
	}
	q, err := e.cfg.Hedge.Venue.Quote(ctx, e.cfg.Hedge.Symbol)
	if err != nil {
		return res, fmt.Errorf("hedge quote: %w", err)
	}
	res.Quote = q
	if spread := q.SpreadBps(); spread.GreaterThan(e.cfg.MaxSpreadBps) {
		return res, fmt.Errorf("hedge spread %s bps exceeds %s", spread.StringFixed(2), e.cfg.MaxSpreadBps)
	}
	if limit := q.TakerDepth(hedgeSide).Mul(e.cfg.MaxBookShare); size.GreaterThan(limit) {
		return res, fmt.Errorf("size %s exceeds %s of top-of-book depth (%s)", size, e.cfg.MaxBookShare, limit)
	}

	res.Hedge, err = e.cfg.Hedge.Venue.PlaceOrder(ctx, venue.OrderRequest{
		Symbol: e.cfg.Hedge.Symbol, Side: hedgeSide, Size: size, MaxSlippage: e.cfg.Slippage,
	})
	if err != nil {
		return res, fmt.Errorf("hedge leg: %w", err)
	}
	if res.Hedge.Status != venue.OrderFilled || !res.Hedge.FilledSize.IsPositive() {
		return res, fmt.Errorf("hedge leg did not fill (%s %s)", res.Hedge.Status, res.Hedge.Error)
	}

	// Size the mint leg by what the hedge actually filled.
	res.Mint, err = e.cfg.Mint.Venue.PlaceOrder(ctx, venue.OrderRequest{
		Symbol: e.cfg.Mint.Symbol, Side: mintSide, Size: res.Hedge.FilledSize,
	})
	if err == nil && res.Mint.Status == venue.OrderRejected {
		err = fmt.Errorf("rejected: %s", res.Mint.Error)
	}
	if err != nil {
		if unwindErr := e.closeHedge(ctx); unwindErr != nil {
			return res, fmt.Errorf("mint leg failed (%v) and hedge unwind failed: %w: %w", err, ErrUnhedged, unwindErr)
		}
		return res, fmt.Errorf("mint leg failed, hedge unwound: %w", err)
	}
	return res, nil
}

type CloseResult struct {
	MintBefore  venue.Position
	HedgeBefore venue.Position
	Hedge       venue.OrderResult
}

// Close flattens the mint leg, waits for it to settle, then the hedge leg.
func (e *Engine) Close(ctx context.Context) (CloseResult, error) {
	var res CloseResult
	var err error
	if res.MintBefore, err = e.cfg.Mint.Venue.Position(ctx, e.cfg.Mint.Symbol); err != nil {
		return res, fmt.Errorf("mint position: %w", err)
	}
	if res.HedgeBefore, err = e.cfg.Hedge.Venue.Position(ctx, e.cfg.Hedge.Symbol); err != nil {
		return res, fmt.Errorf("hedge position: %w", err)
	}
	if !res.MintBefore.Size.IsZero() {
		if _, err := e.cfg.Mint.Venue.PlaceOrder(ctx, venue.OrderRequest{
			Symbol: e.cfg.Mint.Symbol, Side: venue.SideForSignedSize(res.MintBefore.Size).Opposite(),
			Size: res.MintBefore.Size.Abs(), ReduceOnly: true,
		}); err != nil {
			return res, fmt.Errorf("mint close: %w", err)
		}
		if err := e.waitFlat(ctx, e.cfg.Mint); err != nil {
			// The hedge stays on: it is what protects the still-open mint leg.
			return res, fmt.Errorf("mint leg not flat, hedge kept open: %w", err)
		}
	}
	if res.HedgeBefore.Size.IsZero() {
		return res, nil
	}
	res.Hedge, err = e.closeHedgeSize(ctx, res.HedgeBefore.Size)
	if err != nil {
		return res, fmt.Errorf("mint is flat but hedge close failed: %w: %w", ErrUnhedged, err)
	}
	return res, nil
}

func (e *Engine) closeHedge(ctx context.Context) error {
	p, err := e.cfg.Hedge.Venue.Position(ctx, e.cfg.Hedge.Symbol)
	if err != nil {
		return err
	}
	if p.Size.IsZero() {
		return nil
	}
	_, err = e.closeHedgeSize(ctx, p.Size)
	return err
}

func (e *Engine) closeHedgeSize(ctx context.Context, signed decimal.Decimal) (venue.OrderResult, error) {
	res, err := e.cfg.Hedge.Venue.PlaceOrder(ctx, venue.OrderRequest{
		Symbol: e.cfg.Hedge.Symbol, Side: venue.SideForSignedSize(signed).Opposite(), Size: signed.Abs(),
		ReduceOnly: true, MaxSlippage: e.cfg.Slippage,
	})
	if err == nil && res.Status != venue.OrderFilled {
		err = fmt.Errorf("hedge close %s: %s", res.Status, res.Error)
	}
	return res, err
}

func (e *Engine) waitFlat(ctx context.Context, leg Leg) error {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.CloseTimeout)
	defer cancel()
	for {
		p, err := leg.Venue.Position(ctx, leg.Symbol)
		if err == nil && p.Size.IsZero() {
			return nil
		}
		if err := e.sleep(ctx, e.cfg.PollInterval); err != nil {
			return fmt.Errorf("waiting for %s %s to close: %w", leg.Venue.Name(), leg.Symbol, err)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
