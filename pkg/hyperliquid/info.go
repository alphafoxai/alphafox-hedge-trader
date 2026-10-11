package hyperliquid

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/shopspring/decimal"
)

// InfoClient reads the public /info endpoint. It covers the main perp dex
// only; HIP-3 dexes (asset = 100000 + dexIndex*10000 + i) are not wired.
type InfoClient struct {
	t transport
}

func NewInfoClient(network Network, client *http.Client) *InfoClient {
	return &InfoClient{t: newTransport(network.APIBaseURL(), client)}
}

// NewInfoClientWithBaseURL targets a custom endpoint (tests, proxies).
func NewInfoClientWithBaseURL(baseURL string, client *http.Client) *InfoClient {
	return &InfoClient{t: newTransport(strings.TrimRight(baseURL, "/"), client)}
}

type AssetMeta struct {
	Name         string `json:"name"`
	SzDecimals   int    `json:"szDecimals"`
	MaxLeverage  int    `json:"maxLeverage"`
	OnlyIsolated bool   `json:"onlyIsolated"`
	IsDelisted   bool   `json:"isDelisted"`
	// Index is the position in the universe, which is the perp asset id.
	Index int `json:"-"`
}

type Meta struct {
	Universe []AssetMeta `json:"universe"`
}

// Asset looks up a coin such as "BTC".
func (m Meta) Asset(coin string) (AssetMeta, error) {
	for _, a := range m.Universe {
		if a.Name == coin {
			if a.IsDelisted {
				return AssetMeta{}, fmt.Errorf("hyperliquid asset %s is delisted", coin)
			}
			return a, nil
		}
	}
	return AssetMeta{}, fmt.Errorf("hyperliquid asset %s not found", coin)
}

func (c *InfoClient) Meta(ctx context.Context) (Meta, error) {
	var m Meta
	if err := c.t.post(ctx, "/info", map[string]any{"type": "meta"}, &m); err != nil {
		return Meta{}, err
	}
	for i := range m.Universe {
		m.Universe[i].Index = i
	}
	return m, nil
}

type BookLevel struct {
	Px decimal.Decimal `json:"px"`
	Sz decimal.Decimal `json:"sz"`
	N  int             `json:"n"`
}

type L2Book struct {
	Coin   string         `json:"coin"`
	Time   int64          `json:"time"`
	Levels [2][]BookLevel `json:"levels"` // [bids, asks]
}

func (c *InfoClient) L2Book(ctx context.Context, coin string) (L2Book, error) {
	var b L2Book
	err := c.t.post(ctx, "/info", map[string]any{"type": "l2Book", "coin": coin}, &b)
	return b, err
}

type AssetPosition struct {
	Position struct {
		Coin          string          `json:"coin"`
		Szi           decimal.Decimal `json:"szi"`
		EntryPx       decimal.Decimal `json:"entryPx"`
		UnrealizedPnl decimal.Decimal `json:"unrealizedPnl"`
		Leverage      struct {
			Type  string `json:"type"`
			Value int    `json:"value"`
		} `json:"leverage"`
	} `json:"position"`
}

type ClearinghouseState struct {
	AssetPositions []AssetPosition `json:"assetPositions"`
	MarginSummary  struct {
		AccountValue decimal.Decimal `json:"accountValue"`
	} `json:"marginSummary"`
	Withdrawable decimal.Decimal `json:"withdrawable"`
}

// ClearinghouseState must be queried with the master account address, not
// the agent wallet address.
func (c *InfoClient) ClearinghouseState(ctx context.Context, user string) (ClearinghouseState, error) {
	var s ClearinghouseState
	err := c.t.post(ctx, "/info", map[string]any{"type": "clearinghouseState", "user": user}, &s)
	return s, err
}

type OpenOrder struct {
	Coin    string          `json:"coin"`
	Side    string          `json:"side"` // "B" or "A"
	LimitPx decimal.Decimal `json:"limitPx"`
	Sz      decimal.Decimal `json:"sz"`
	Oid     uint64          `json:"oid"`
}

func (c *InfoClient) OpenOrders(ctx context.Context, user string) ([]OpenOrder, error) {
	var orders []OpenOrder
	err := c.t.post(ctx, "/info", map[string]any{"type": "openOrders", "user": user}, &orders)
	return orders, err
}

// Candle is one candleSnapshot bar.
type Candle struct {
	OpenTime  int64           `json:"t"`
	CloseTime int64           `json:"T"`
	Open      decimal.Decimal `json:"o"`
	High      decimal.Decimal `json:"h"`
	Low       decimal.Decimal `json:"l"`
	Close     decimal.Decimal `json:"c"`
	Interval  string          `json:"i"`
	Trades    int             `json:"n"`
}

// Candles returns bars for interval ("1m", "5m", "1h", ...) between start and
// end (unix ms), oldest first. The API caps one response at 5000 bars.
func (c *InfoClient) Candles(ctx context.Context, coin, interval string, start, end int64) ([]Candle, error) {
	var out []Candle
	err := c.t.post(ctx, "/info", map[string]any{"type": "candleSnapshot",
		"req": map[string]any{"coin": coin, "interval": interval, "startTime": start, "endTime": end}}, &out)
	return out, err
}

// Mid is the top-of-book mid, the price Papertrade settles at.
func (c *InfoClient) Mid(ctx context.Context, coin string) (decimal.Decimal, error) {
	b, err := c.L2Book(ctx, coin)
	if err != nil {
		return decimal.Zero, err
	}
	if len(b.Levels[0]) == 0 || len(b.Levels[1]) == 0 {
		return decimal.Zero, fmt.Errorf("hyperliquid %s book is one-sided", coin)
	}
	return b.Levels[0][0].Px.Add(b.Levels[1][0].Px).Div(decimal.NewFromInt(2)), nil
}
