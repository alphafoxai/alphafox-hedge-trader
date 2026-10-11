// Package venue defines the perp venue seam that hedge orchestration trades
// through. Hyperliquid and Paper each implement Venue; strategy code never
// imports a concrete venue package.
package venue

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// ErrNotImplemented marks a venue capability that is reserved but not wired.
var ErrNotImplemented = errors.New("venue capability not implemented")

type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Opposite returns the side that offsets s.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// SideForSignedSize returns the side that opens a position of the given sign.
func SideForSignedSize(size decimal.Decimal) Side {
	if size.IsNegative() {
		return Sell
	}
	return Buy
}

type TimeInForce string

const (
	// IOC fills what it can immediately and cancels the rest. A market order is
	// an IOC limit at a slippage-protected price.
	IOC TimeInForce = "Ioc"
	GTC TimeInForce = "Gtc"
	// ALO (add-liquidity-only) is post-only.
	ALO TimeInForce = "Alo"
)

// OrderRequest is venue-neutral. A zero LimitPrice means a market order: the
// venue derives an aggressive IOC price from its book and MaxSlippage.
type OrderRequest struct {
	Symbol        string
	Side          Side
	Size          decimal.Decimal
	LimitPrice    decimal.Decimal
	TimeInForce   TimeInForce
	ReduceOnly    bool
	ClientOrderID string
	// MaxSlippage is a fraction (0.01 = 1%) used only for market orders.
	MaxSlippage decimal.Decimal
}

func (r OrderRequest) IsMarket() bool { return r.LimitPrice.IsZero() }

type OrderStatus string

const (
	OrderFilled   OrderStatus = "filled"
	OrderResting  OrderStatus = "resting"
	OrderRejected OrderStatus = "rejected"
	// OrderPending covers venues that queue execution (Paper closes queue).
	OrderPending OrderStatus = "pending"
)

type OrderResult struct {
	OrderID    string
	Status     OrderStatus
	FilledSize decimal.Decimal
	AvgPrice   decimal.Decimal
	Error      string
}

// Position is signed: positive is long, negative is short, zero is flat.
type Position struct {
	Symbol        string
	Size          decimal.Decimal
	EntryPrice    decimal.Decimal
	UnrealizedPnL decimal.Decimal
}

// Quote is the top of book.
type Quote struct {
	Symbol  string
	Bid     decimal.Decimal
	BidSize decimal.Decimal
	Ask     decimal.Decimal
	AskSize decimal.Decimal
	Time    time.Time
}

func (q Quote) Mid() decimal.Decimal { return q.Bid.Add(q.Ask).Div(decimal.NewFromInt(2)) }

// SpreadBps is (ask - bid) / mid in basis points.
func (q Quote) SpreadBps() decimal.Decimal {
	mid := q.Mid()
	if mid.IsZero() {
		return decimal.Zero
	}
	return q.Ask.Sub(q.Bid).Div(mid).Mul(decimal.NewFromInt(10_000))
}

// TakerDepth is the top-level size a taker on side would consume.
func (q Quote) TakerDepth(side Side) decimal.Decimal {
	if side == Buy {
		return q.AskSize
	}
	return q.BidSize
}

type Venue interface {
	Name() string
	Quote(ctx context.Context, symbol string) (Quote, error)
	PlaceOrder(ctx context.Context, req OrderRequest) (OrderResult, error)
	CancelOrder(ctx context.Context, symbol, orderID string) error
	// Position returns a zero-size Position when flat, never an error.
	Position(ctx context.Context, symbol string) (Position, error)
	SetLeverage(ctx context.Context, symbol string, leverage int, cross bool) error
}
