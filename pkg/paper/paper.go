// Package paper is the reserved adapter for the Paper perp venue: a
// loss-mining perp that settles against Hyperliquid prices, lists BTC and
// ETH up to 1000x, has no public API, and is driven by wallet signatures.
//
// The on-chain flow is not known yet (see docs/paper-integration.md). Until it
// is, Venue routes every call through a Gateway; with no Gateway it returns
// venue.ErrNotImplemented so a hedge can never open a one-legged position by
// accident.
package paper

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

// MaxLeverage is Paper's advertised ceiling.
const MaxLeverage = 1000

// Symbols Paper lists. Names follow Hyperliquid coins so a hedge pair shares
// one symbol.
var Symbols = []string{"BTC", "ETH"}

// Gateway is what the on-chain integration has to provide. It is shaped by
// what Paper's UI exposes (open with leverage, close whole position, read
// position and mark), not by an order book: Paper has none.
type Gateway interface {
	// Open adds size (base units, signed by side) at the venue price.
	Open(ctx context.Context, symbol string, side venue.Side, size decimal.Decimal, leverage int) (venue.OrderResult, error)
	// Close requests a full close. Paper queues closes, so OrderPending is a
	// normal result; callers confirm with Position.
	Close(ctx context.Context, symbol string) (venue.OrderResult, error)
	Position(ctx context.Context, symbol string) (venue.Position, error)
	MarkPrice(ctx context.Context, symbol string) (decimal.Decimal, error)
}

type Config struct {
	// Leverage is applied to every Open. Higher leverage needs less margin
	// but liquidates sooner if the hedge leg is late.
	Leverage int
}

type Venue struct {
	cfg     Config
	gateway Gateway
}

var _ venue.Venue = (*Venue)(nil)

// NewVenue accepts a nil gateway: the venue is then a placeholder whose
// every call fails with venue.ErrNotImplemented.
func NewVenue(cfg Config, gateway Gateway) (*Venue, error) {
	if cfg.Leverage < 1 || cfg.Leverage > MaxLeverage {
		return nil, fmt.Errorf("paper leverage %d outside 1..%d", cfg.Leverage, MaxLeverage)
	}
	return &Venue{cfg: cfg, gateway: gateway}, nil
}

func (v *Venue) Name() string { return "paper" }

func (v *Venue) ready(op string) error {
	if v.gateway == nil {
		return fmt.Errorf("paper %s: %w", op, venue.ErrNotImplemented)
	}
	return nil
}

// Quote reports the mark as both sides: Paper fills at the venue price with
// no book, so there is no spread or depth to check.
func (v *Venue) Quote(ctx context.Context, symbol string) (venue.Quote, error) {
	if err := v.ready("quote"); err != nil {
		return venue.Quote{}, err
	}
	mark, err := v.gateway.MarkPrice(ctx, symbol)
	if err != nil {
		return venue.Quote{}, err
	}
	return venue.Quote{Symbol: symbol, Bid: mark, Ask: mark}, nil
}

// PlaceOrder maps reduce-only orders to Close (Paper closes whole positions)
// and everything else to Open. Limit prices are not supported.
func (v *Venue) PlaceOrder(ctx context.Context, req venue.OrderRequest) (venue.OrderResult, error) {
	if err := v.ready("place order"); err != nil {
		return venue.OrderResult{}, err
	}
	if !req.IsMarket() {
		return venue.OrderResult{}, fmt.Errorf("paper limit orders: %w", venue.ErrNotImplemented)
	}
	if req.ReduceOnly {
		return v.gateway.Close(ctx, req.Symbol)
	}
	return v.gateway.Open(ctx, req.Symbol, req.Side, req.Size, v.cfg.Leverage)
}

func (v *Venue) CancelOrder(ctx context.Context, symbol, orderID string) error {
	return fmt.Errorf("paper cancel: %w", venue.ErrNotImplemented)
}

func (v *Venue) Position(ctx context.Context, symbol string) (venue.Position, error) {
	if err := v.ready("position"); err != nil {
		return venue.Position{}, err
	}
	return v.gateway.Position(ctx, symbol)
}

// SetLeverage is per-open on Paper; it only updates the configured value.
func (v *Venue) SetLeverage(ctx context.Context, symbol string, leverage int, cross bool) error {
	if leverage < 1 || leverage > MaxLeverage {
		return fmt.Errorf("paper leverage %d outside 1..%d", leverage, MaxLeverage)
	}
	v.cfg.Leverage = leverage
	return nil
}
