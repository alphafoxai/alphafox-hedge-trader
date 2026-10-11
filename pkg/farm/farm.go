// Package farm runs Paper's same-platform loss farming: open an equal long
// and short, wait until price has moved far enough that the losing leg mints
// meaningful PAPER, then close both legs with one Close intent so they settle
// in the same block at the same price. The winner's impact haircut and win
// fee are the cost; the loser's loss mints PAPER.
//
// Safety invariants (tests pin each):
//   - both legs are matched by their own intent id; a pair is never
//     inferred from position shape;
//   - if only one leg opens, it is closed before the cycle reports failure;
//   - both legs always close together in one intent;
//   - the close trigger sits below the bust distance, and a bust guard closes
//     early if either leg gets too near its bust price.
package farm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Position is an open Paper position as the strategy sees it.
type Position struct {
	ID       string          `json:"id"`
	IntentID string          `json:"intentId"`
	Symbol   string          `json:"symbol"`
	IsLong   bool            `json:"isLong"`
	Entry    decimal.Decimal `json:"entry"`
	Bust     decimal.Decimal `json:"bust"`
	Margin   decimal.Decimal `json:"margin"`
	Leverage int             `json:"leverage"`
}

// Settled is one closed position from the trade history.
type Settled struct {
	PositionID  string          `json:"positionId"`
	IsLong      bool            `json:"isLong"`
	Kind        string          `json:"kind"`
	Exit        decimal.Decimal `json:"exit"`
	PnL         decimal.Decimal `json:"pnl"`
	Fee         decimal.Decimal `json:"fee"`
	PaperMinted decimal.Decimal `json:"paperMinted"`
}

// Exchange is what the strategy needs from Paper. Open and Close return the
// intent id the relayer accepted; acceptance is not execution.
type Exchange interface {
	Open(ctx context.Context, symbol string, isLong bool, margin decimal.Decimal, leverage int) (string, error)
	Close(ctx context.Context, positionIDs []string) (string, error)
	Positions(ctx context.Context) ([]Position, error)
	Settled(ctx context.Context, positionIDs []string) ([]Settled, error)
}

// PriceFunc returns the price Paper settles against (the Hyperliquid BBO mid).
type PriceFunc func(ctx context.Context, symbol string) (decimal.Decimal, error)

type Config struct {
	Symbol   string
	Margin   decimal.Decimal // per leg, USD
	Leverage int
	// CloseMove is the adverse move (fraction of entry) on the losing leg
	// that triggers the paired close.
	CloseMove decimal.Decimal
	// BustGuard closes both legs once either leg is within this fraction of
	// price from its bust price, whatever CloseMove says.
	BustGuard decimal.Decimal
	// MaxHold closes the pair after this long even below CloseMove; zero
	// holds until a trigger.
	MaxHold time.Duration
	// FillTimeout bounds the wait for both opens. Keep it longer than the
	// intent TTL so a late leg cannot execute after the rescue.
	FillTimeout  time.Duration
	CloseTimeout time.Duration
	CloseRetries int
	PollInterval time.Duration
	// MinMargin and MinNotional are the venue minimums (from /state/trading).
	MinMargin   decimal.Decimal
	MinNotional decimal.Decimal
}

// Validate checks the config against the venue minimums and the bust distance.
func (c Config) Validate() error {
	switch {
	case c.Symbol == "":
		return errors.New("farm: symbol is required")
	case c.Leverage < 1:
		return fmt.Errorf("farm: leverage %d must be positive", c.Leverage)
	case !c.Margin.IsPositive():
		return errors.New("farm: margin must be positive")
	case c.Margin.LessThan(c.MinMargin):
		return fmt.Errorf("farm: margin %s below venue minimum %s", c.Margin, c.MinMargin)
	case c.Margin.Mul(decimal.NewFromInt(int64(c.Leverage))).LessThan(c.MinNotional):
		return fmt.Errorf("farm: notional %s below venue minimum %s; raise margin or leverage",
			c.Margin.Mul(decimal.NewFromInt(int64(c.Leverage))), c.MinNotional)
	case !c.CloseMove.IsPositive():
		return errors.New("farm: close move must be positive")
	case c.BustGuard.IsNegative():
		return errors.New("farm: bust guard must not be negative")
	case c.PollInterval <= 0 || c.FillTimeout <= 0 || c.CloseTimeout <= 0:
		return errors.New("farm: poll interval and timeouts must be positive")
	}
	// bust ≈ 1/leverage − 5 bp; the trigger plus guard must fit inside it.
	bust := decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(c.Leverage))).Sub(decimal.RequireFromString("0.0005"))
	if c.CloseMove.Add(c.BustGuard).GreaterThanOrEqual(bust) {
		return fmt.Errorf("farm: close move %s + bust guard %s must stay below the ~%s bust distance at %dx",
			c.CloseMove, c.BustGuard, bust.StringFixed(5), c.Leverage)
	}
	return nil
}

// Close reasons.
const (
	ReasonThreshold = "threshold"
	ReasonBustGuard = "bust-guard"
	ReasonMaxHold   = "max-hold"
	ReasonLegGone   = "leg-gone"
)

// Result reports one cycle. NetCost is what the pair cost (loss minus the
// winner's credit); CostPerPaper divides it by PAPER minted.
type Result struct {
	Long         Position        `json:"long"`
	Short        Position        `json:"short"`
	Reason       string          `json:"reason"`
	CloseIntent  string          `json:"closeIntent"`
	ClosePrice   decimal.Decimal `json:"closeTriggerPrice"`
	Settled      []Settled       `json:"settled"`
	NetCost      decimal.Decimal `json:"netCost"`
	PaperMinted  decimal.Decimal `json:"paperMinted"`
	CostPerPaper decimal.Decimal `json:"costPerPaper"`
	OpenedAt     time.Time       `json:"openedAt"`
	ClosedAt     time.Time       `json:"closedAt"`
}

// ErrOneLegged means only one leg opened; it was closed (see the wrapped
// error if that also failed).
var ErrOneLegged = errors.New("farm: only one leg opened")

// ErrNotFlat means the account already has positions on the symbol.
var ErrNotFlat = errors.New("farm: account already has open positions on this symbol")

type Farmer struct {
	cfg   Config
	ex    Exchange
	price PriceFunc
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	log   func(format string, args ...any)
}

// Option overrides the clock or logger, mainly for tests.
type Option func(*Farmer)

func WithClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(f *Farmer) { f.now, f.sleep = now, sleep }
}

func WithLogger(log func(format string, args ...any)) Option {
	return func(f *Farmer) { f.log = log }
}

func New(cfg Config, ex Exchange, price PriceFunc, opts ...Option) (*Farmer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if ex == nil || price == nil {
		return nil, errors.New("farm: exchange and price source are required")
	}
	f := &Farmer{cfg: cfg, ex: ex, price: price, now: time.Now, sleep: sleepCtx, log: func(string, ...any) {}}
	for _, o := range opts {
		o(f)
	}
	return f, nil
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

// RunCycle opens one pair, waits for a trigger, closes both legs together and
// reports what settled. If ctx is cancelled while the pair is open, the
// positions are left open (they hedge each other) and returned in the error.
func (f *Farmer) RunCycle(ctx context.Context) (Result, error) {
	open, err := f.ex.Positions(ctx)
	if err != nil {
		return Result{}, err
	}
	for _, p := range open {
		if p.Symbol == f.cfg.Symbol {
			return Result{}, fmt.Errorf("%w (position %s)", ErrNotFlat, p.ID)
		}
	}

	long, short, err := f.openPair(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Long: long, Short: short, OpenedAt: f.now()}
	f.log("opened long %s @ %s (bust %s), short %s @ %s (bust %s)", long.ID, long.Entry, long.Bust, short.ID, short.Entry, short.Bust)

	res.Reason, res.ClosePrice, err = f.watch(ctx, long, short)
	if err != nil {
		return res, fmt.Errorf("farm: pair %s/%s left open: %w", long.ID, short.ID, err)
	}
	f.log("closing pair: %s at %s", res.Reason, res.ClosePrice)
	if res.CloseIntent, err = f.closeAll(ctx, []string{long.ID, short.ID}); err != nil {
		return res, err
	}
	res.ClosedAt = f.now()
	if res.Settled, err = f.settled(ctx, []string{long.ID, short.ID}); err != nil {
		return res, err
	}
	for _, s := range res.Settled {
		res.NetCost = res.NetCost.Sub(s.PnL)
		res.PaperMinted = res.PaperMinted.Add(s.PaperMinted)
	}
	if res.PaperMinted.IsPositive() {
		res.CostPerPaper = res.NetCost.Div(res.PaperMinted)
	}
	return res, nil
}

// openPair submits both opens back to back, then waits until both show up.
// If only one does, it is closed before returning ErrOneLegged.
func (f *Farmer) openPair(ctx context.Context) (Position, Position, error) {
	longIntent, err := f.ex.Open(ctx, f.cfg.Symbol, true, f.cfg.Margin, f.cfg.Leverage)
	if err != nil {
		return Position{}, Position{}, fmt.Errorf("farm: open long: %w", err)
	}
	shortIntent, shortErr := f.ex.Open(ctx, f.cfg.Symbol, false, f.cfg.Margin, f.cfg.Leverage)
	want := map[string]bool{longIntent: true}
	if shortErr == nil {
		want[shortIntent] = true
	}

	found, err := f.waitIntents(ctx, want)
	if err != nil && len(found) == 0 {
		return Position{}, Position{}, fmt.Errorf("farm: waiting for opens: %w", err)
	}
	long, haveLong := found[longIntent]
	short, haveShort := found[shortIntent]
	if haveLong && haveShort && shortErr == nil {
		return long, short, nil
	}

	cause := shortErr
	if cause == nil {
		cause = err
	}
	if cause == nil {
		cause = errors.New("fill timeout")
	}
	var ids []string
	for _, p := range found {
		ids = append(ids, p.ID)
	}
	if len(ids) == 0 {
		return Position{}, Position{}, fmt.Errorf("farm: neither leg opened: %w", cause)
	}
	f.log("one leg opened (%v); closing it", ids)
	if _, closeErr := f.closeAll(context.WithoutCancel(ctx), ids); closeErr != nil {
		return Position{}, Position{}, fmt.Errorf("%w: %v; rescue close of %v failed: %w", ErrOneLegged, cause, ids, closeErr)
	}
	return Position{}, Position{}, fmt.Errorf("%w and was closed: %v", ErrOneLegged, cause)
}

// waitIntents polls until every wanted intent has a position or FillTimeout
// passes, returning what it found either way.
func (f *Farmer) waitIntents(ctx context.Context, want map[string]bool) (map[string]Position, error) {
	deadline := f.now().Add(f.cfg.FillTimeout)
	found := map[string]Position{}
	for {
		ps, err := f.ex.Positions(ctx)
		if err != nil {
			f.log("positions: %v", err)
		}
		for _, p := range ps {
			if want[p.IntentID] {
				found[p.IntentID] = p
			}
		}
		if len(found) == len(want) {
			return found, nil
		}
		if !f.now().Before(deadline) {
			return found, errors.New("fill timeout")
		}
		if err := f.sleep(ctx, f.cfg.PollInterval); err != nil {
			return found, err
		}
	}
}

// watch polls price and positions until a close trigger.
func (f *Farmer) watch(ctx context.Context, long, short Position) (string, decimal.Decimal, error) {
	start := f.now()
	for {
		px, err := f.price(ctx, f.cfg.Symbol)
		if err != nil {
			f.log("price: %v", err)
		} else if reason := f.trigger(long, short, px, f.now().Sub(start)); reason != "" {
			return reason, px, nil
		}
		ps, err := f.ex.Positions(ctx)
		if err == nil && !(has(ps, long.ID) && has(ps, short.ID)) {
			return ReasonLegGone, px, nil
		}
		if err := f.sleep(ctx, f.cfg.PollInterval); err != nil {
			return "", decimal.Zero, err
		}
	}
}

func (f *Farmer) trigger(long, short Position, px decimal.Decimal, held time.Duration) string {
	// Adverse move on each leg, as a fraction of its own entry.
	longLoss := long.Entry.Sub(px).Div(long.Entry)
	shortLoss := px.Sub(short.Entry).Div(short.Entry)
	// Distance to bust as a fraction of price.
	longRoom := px.Sub(long.Bust).Div(px)
	shortRoom := short.Bust.Sub(px).Div(px)
	switch {
	case longRoom.LessThanOrEqual(f.cfg.BustGuard) || shortRoom.LessThanOrEqual(f.cfg.BustGuard):
		return ReasonBustGuard
	case decimal.Max(longLoss, shortLoss).GreaterThanOrEqual(f.cfg.CloseMove):
		return ReasonThreshold
	case f.cfg.MaxHold > 0 && held >= f.cfg.MaxHold:
		return ReasonMaxHold
	}
	return ""
}

// closeAll submits one Close for every id and waits until all are gone,
// resubmitting the still-open ids after each CloseTimeout.
func (f *Farmer) closeAll(ctx context.Context, ids []string) (string, error) {
	var intent string
	pending := ids
	for attempt := 0; attempt <= f.cfg.CloseRetries; attempt++ {
		id, err := f.ex.Close(ctx, pending)
		if err != nil {
			f.log("close %v attempt %d: %v", pending, attempt+1, err)
		} else if intent == "" {
			intent = id
		}
		deadline := f.now().Add(f.cfg.CloseTimeout)
		for {
			ps, perr := f.ex.Positions(ctx)
			if perr == nil {
				pending = pending[:0:0]
				for _, want := range ids {
					if has(ps, want) {
						pending = append(pending, want)
					}
				}
				if len(pending) == 0 {
					return intent, nil
				}
			}
			if !f.now().Before(deadline) {
				break
			}
			if err := f.sleep(ctx, f.cfg.PollInterval); err != nil {
				return intent, err
			}
		}
	}
	return intent, fmt.Errorf("farm: positions %v still open after %d close attempts", pending, f.cfg.CloseRetries+1)
}

// settled waits for the history to index every closed id.
func (f *Farmer) settled(ctx context.Context, ids []string) ([]Settled, error) {
	deadline := f.now().Add(f.cfg.CloseTimeout)
	for {
		got, err := f.ex.Settled(ctx, ids)
		if err == nil && len(got) == len(ids) {
			return got, nil
		}
		if !f.now().Before(deadline) {
			if err == nil {
				err = fmt.Errorf("history shows %d of %d closes", len(got), len(ids))
			}
			return got, fmt.Errorf("farm: closed but settlement unconfirmed: %w", err)
		}
		if err := f.sleep(ctx, f.cfg.PollInterval); err != nil {
			return got, err
		}
	}
}

func has(ps []Position, id string) bool {
	for _, p := range ps {
		if p.ID == id {
			return true
		}
	}
	return false
}
