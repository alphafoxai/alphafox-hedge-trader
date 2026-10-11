package paper

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

// PriceSource supplies the reference price Paper settles against; in
// practice the Hyperliquid mid.
type PriceSource func(ctx context.Context, symbol string) (decimal.Decimal, error)

// SimulatedGateway rehearses Paper's behavior, including queued closes, so
// the hedge sequencing can be exercised before the real integration exists.
// It holds no funds and talks to no chain.
type SimulatedGateway struct {
	price      PriceSource
	closeDelay time.Duration
	now        func() time.Time

	mu        sync.Mutex
	positions map[string]venue.Position
	closingAt map[string]time.Time
}

// NewSimulatedGateway fills opens at the reference price and settles a close
// closeDelay after it is requested.
func NewSimulatedGateway(price PriceSource, closeDelay time.Duration, now func() time.Time) *SimulatedGateway {
	if now == nil {
		now = time.Now
	}
	return &SimulatedGateway{price: price, closeDelay: closeDelay, now: now,
		positions: map[string]venue.Position{}, closingAt: map[string]time.Time{}}
}

func (g *SimulatedGateway) Open(ctx context.Context, symbol string, side venue.Side, size decimal.Decimal, leverage int) (venue.OrderResult, error) {
	px, err := g.price(ctx, symbol)
	if err != nil {
		return venue.OrderResult{}, err
	}
	signed := size
	if side == venue.Sell {
		signed = size.Neg()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, closing := g.closingAt[symbol]; closing {
		return venue.OrderResult{}, fmt.Errorf("paper %s close is still queued", symbol)
	}
	p := g.positions[symbol]
	if p.Size.IsZero() {
		p = venue.Position{Symbol: symbol, EntryPrice: px}
	}
	p.Size = p.Size.Add(signed)
	g.positions[symbol] = p
	return venue.OrderResult{OrderID: fmt.Sprintf("sim-%d", g.now().UnixNano()), Status: venue.OrderFilled, FilledSize: size, AvgPrice: px}, nil
}

func (g *SimulatedGateway) Close(ctx context.Context, symbol string) (venue.OrderResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.positions[symbol].Size.IsZero() {
		return venue.OrderResult{Status: venue.OrderFilled}, nil
	}
	if _, ok := g.closingAt[symbol]; !ok {
		g.closingAt[symbol] = g.now().Add(g.closeDelay)
	}
	return venue.OrderResult{Status: venue.OrderPending}, nil
}

func (g *SimulatedGateway) Position(ctx context.Context, symbol string) (venue.Position, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if at, ok := g.closingAt[symbol]; ok && !g.now().Before(at) {
		delete(g.closingAt, symbol)
		delete(g.positions, symbol)
	}
	if p, ok := g.positions[symbol]; ok {
		return p, nil
	}
	return venue.Position{Symbol: symbol}, nil
}

func (g *SimulatedGateway) MarkPrice(ctx context.Context, symbol string) (decimal.Decimal, error) {
	return g.price(ctx, symbol)
}
