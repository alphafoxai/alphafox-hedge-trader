package hyperliquid

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

// VenueConfig binds one trading account.
type VenueConfig struct {
	// AccountAddress is the master wallet whose positions the agent trades.
	// Positions are read from it, not from the agent signer's address.
	AccountAddress string
	// Builder, when set, attaches a builder fee to every order. The master
	// wallet must have approved at least that rate.
	Builder *BuilderWire
	// DefaultSlippage applies to market orders that leave MaxSlippage zero.
	DefaultSlippage decimal.Decimal
}

// Venue adapts InfoClient + ExchangeClient to venue.Venue. Symbols are
// Hyperliquid coin names ("BTC", "ETH").
type Venue struct {
	cfg      VenueConfig
	info     *InfoClient
	exchange *ExchangeClient

	metaOnce sync.Once
	meta     Meta
	metaErr  error
}

var _ venue.Venue = (*Venue)(nil)

func NewVenue(cfg VenueConfig, info *InfoClient, exchange *ExchangeClient) (*Venue, error) {
	if cfg.AccountAddress == "" {
		return nil, fmt.Errorf("hyperliquid venue needs the master account address")
	}
	if cfg.Builder != nil {
		b := *cfg.Builder
		b.B = strings.ToLower(b.B)
		cfg.Builder = &b
	}
	return &Venue{cfg: cfg, info: info, exchange: exchange}, nil
}

func (v *Venue) Name() string { return "hyperliquid" }

func (v *Venue) asset(ctx context.Context, coin string) (AssetMeta, error) {
	v.metaOnce.Do(func() { v.meta, v.metaErr = v.info.Meta(ctx) })
	if v.metaErr != nil {
		return AssetMeta{}, v.metaErr
	}
	return v.meta.Asset(coin)
}

func (v *Venue) Quote(ctx context.Context, symbol string) (venue.Quote, error) {
	book, err := v.info.L2Book(ctx, symbol)
	if err != nil {
		return venue.Quote{}, err
	}
	bids, asks := book.Levels[0], book.Levels[1]
	if len(bids) == 0 || len(asks) == 0 {
		return venue.Quote{}, fmt.Errorf("hyperliquid %s book is one-sided", symbol)
	}
	return venue.Quote{
		Symbol: symbol, Bid: bids[0].Px, BidSize: bids[0].Sz, Ask: asks[0].Px, AskSize: asks[0].Sz,
		Time: time.UnixMilli(book.Time),
	}, nil
}

func (v *Venue) PlaceOrder(ctx context.Context, req venue.OrderRequest) (venue.OrderResult, error) {
	asset, err := v.asset(ctx, req.Symbol)
	if err != nil {
		return venue.OrderResult{}, err
	}
	size := RoundSizeDown(req.Size, asset.SzDecimals)
	if !size.IsPositive() {
		return venue.OrderResult{}, fmt.Errorf("size %s rounds to zero at %d decimals", req.Size, asset.SzDecimals)
	}
	px, tif := req.LimitPrice, req.TimeInForce
	if req.IsMarket() {
		if px, err = v.marketPrice(ctx, req); err != nil {
			return venue.OrderResult{}, err
		}
		tif = venue.IOC
	}
	if tif == "" {
		tif = venue.GTC
	}
	pxWire, err := FloatToWire(RoundPerpPrice(px, asset.SzDecimals))
	if err != nil {
		return venue.OrderResult{}, err
	}
	szWire, err := FloatToWire(size)
	if err != nil {
		return venue.OrderResult{}, err
	}
	wire := OrderWire{
		Asset: asset.Index, IsBuy: req.Side == venue.Buy, Price: pxWire, Size: szWire,
		ReduceOnly: req.ReduceOnly, Type: OrderTypeWire{Limit: &LimitWire{TIF: string(tif)}}, Cloid: req.ClientOrderID,
	}
	resp, err := v.exchange.Submit(ctx, NewOrderAction([]OrderWire{wire}, v.cfg.Builder))
	if err != nil {
		return venue.OrderResult{}, err
	}
	if resp.Status == "dry_run" {
		return venue.OrderResult{Status: venue.OrderPending, Error: "dry run: not submitted"}, nil
	}
	statuses, err := resp.OrderStatuses()
	if err != nil {
		return venue.OrderResult{}, err
	}
	if len(statuses) != 1 {
		return venue.OrderResult{}, fmt.Errorf("expected 1 order status, got %d", len(statuses))
	}
	return orderResult(statuses[0]), nil
}

func orderResult(s OrderStatusWire) venue.OrderResult {
	switch {
	case s.Filled != nil:
		// A malformed number decodes to zero, which callers treat as unfilled.
		filled, _ := decimal.NewFromString(s.Filled.TotalSz)
		avg, _ := decimal.NewFromString(s.Filled.AvgPx)
		return venue.OrderResult{
			OrderID: strconv.FormatUint(s.Filled.Oid, 10), Status: venue.OrderFilled, FilledSize: filled, AvgPrice: avg,
		}
	case s.Resting != nil:
		return venue.OrderResult{OrderID: strconv.FormatUint(s.Resting.Oid, 10), Status: venue.OrderResting}
	default:
		return venue.OrderResult{Status: venue.OrderRejected, Error: s.Error}
	}
}

// marketPrice crosses the top of book by the slippage fraction.
func (v *Venue) marketPrice(ctx context.Context, req venue.OrderRequest) (decimal.Decimal, error) {
	slippage := req.MaxSlippage
	if slippage.IsZero() {
		slippage = v.cfg.DefaultSlippage
	}
	if !slippage.IsPositive() {
		return decimal.Zero, fmt.Errorf("market order on %s needs a positive slippage", req.Symbol)
	}
	q, err := v.Quote(ctx, req.Symbol)
	if err != nil {
		return decimal.Zero, err
	}
	one := decimal.NewFromInt(1)
	if req.Side == venue.Buy {
		return q.Ask.Mul(one.Add(slippage)), nil
	}
	return q.Bid.Mul(one.Sub(slippage)), nil
}

func (v *Venue) CancelOrder(ctx context.Context, symbol, orderID string) error {
	asset, err := v.asset(ctx, symbol)
	if err != nil {
		return err
	}
	oid, err := strconv.ParseUint(orderID, 10, 64)
	if err != nil {
		return fmt.Errorf("hyperliquid order id %q: %w", orderID, err)
	}
	resp, err := v.exchange.Submit(ctx, NewCancelAction([]CancelWire{{Asset: asset.Index, Oid: oid}}))
	if err != nil || resp.Status == "dry_run" {
		return err
	}
	// Cancel statuses are "success" or {"error": "..."}.
	var body struct {
		Data struct {
			Statuses []any `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Response, &body); err != nil {
		return err
	}
	for _, s := range body.Data.Statuses {
		if m, ok := s.(map[string]any); ok && m["error"] != nil {
			return fmt.Errorf("hyperliquid cancel %s: %v", orderID, m["error"])
		}
	}
	return nil
}

func (v *Venue) Position(ctx context.Context, symbol string) (venue.Position, error) {
	state, err := v.info.ClearinghouseState(ctx, v.cfg.AccountAddress)
	if err != nil {
		return venue.Position{}, err
	}
	for _, ap := range state.AssetPositions {
		if ap.Position.Coin == symbol {
			return venue.Position{
				Symbol: symbol, Size: ap.Position.Szi, EntryPrice: ap.Position.EntryPx, UnrealizedPnL: ap.Position.UnrealizedPnl,
			}, nil
		}
	}
	return venue.Position{Symbol: symbol}, nil
}

func (v *Venue) SetLeverage(ctx context.Context, symbol string, leverage int, cross bool) error {
	asset, err := v.asset(ctx, symbol)
	if err != nil {
		return err
	}
	if leverage < 1 || leverage > asset.MaxLeverage {
		return fmt.Errorf("leverage %d outside 1..%d for %s", leverage, asset.MaxLeverage, symbol)
	}
	if cross && asset.OnlyIsolated {
		return fmt.Errorf("%s only supports isolated margin", symbol)
	}
	_, err = v.exchange.Submit(ctx, NewUpdateLeverageAction(asset.Index, cross, leverage))
	return err
}
