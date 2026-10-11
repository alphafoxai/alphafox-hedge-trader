package paper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

// Client talks to the Papertrade relayer and its read API. With DryRun set,
// Submit returns the locally computed intent id without posting.
type Client struct {
	baseURL string
	build   string
	http    *http.Client
	DryRun  bool
}

// NewClient targets baseURL (DefaultRelayerURL in production). An empty
// build uses DefaultBuild.
func NewClient(baseURL, build string, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if build == "" {
		build = DefaultBuild
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), build: build, http: client}
}

// SubmitResult is the relayer's acceptance. Acceptance means queued, not
// executed: confirm through the wallet state.
type SubmitResult struct {
	IntentID      string `json:"intentId"`
	Action        string `json:"action"`
	State         string `json:"state"`
	QueueRevision string `json:"queueRevision,omitempty"`
	DryRun        bool   `json:"dryRun,omitempty"`
}

// RelayerError is a rejection with the relayer's code and message.
type RelayerError struct {
	Status  int
	Code    string
	Message string
}

func (e *RelayerError) Error() string {
	return fmt.Sprintf("paper relayer HTTP %d: %s: %s", e.Status, e.Code, e.Message)
}

// Submit posts a signed intent. It fails if the relayer reports a different
// intent id, which would mean the local signing domain has drifted.
func (c *Client) Submit(ctx context.Context, in Intent) (SubmitResult, error) {
	if c.DryRun {
		return SubmitResult{IntentID: in.ID.Hex(), Action: string(in.Action), State: "dry-run", DryRun: true}, nil
	}
	body, err := json.Marshal(in)
	if err != nil {
		return SubmitResult{}, err
	}
	var res struct {
		SubmitResult
		OK      *bool  `json:"ok"`
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	status, err := c.do(ctx, http.MethodPost, "/intents", body, &res)
	if err != nil && status == 0 {
		return SubmitResult{}, err
	}
	if status/100 != 2 || res.Error != "" || (res.OK != nil && !*res.OK) {
		code := res.Code
		if code == "" {
			code = res.Error
		}
		return SubmitResult{}, &RelayerError{Status: status, Code: code, Message: res.Message}
	}
	if err != nil {
		return SubmitResult{}, err
	}
	if !strings.EqualFold(res.IntentID, in.ID.Hex()) {
		return res.SubmitResult, fmt.Errorf("paper relayer intent id %s differs from local %s", res.IntentID, in.ID.Hex())
	}
	return res.SubmitResult, nil
}

// Position is an open position. The wire form is a positional array.
type Position struct {
	ID            string
	InstrumentID  uint32
	IsLong        bool
	UseDebt       bool
	EntryPriceRaw string
	MarginRaw     string
	Leverage      int
	BustPriceRaw  string
	OpenedAtMs    int64
	IntentID      string
}

func (p *Position) UnmarshalJSON(b []byte) error {
	var t []json.RawMessage
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	if len(t) < 10 {
		return fmt.Errorf("paper position tuple has %d fields, want 10", len(t))
	}
	targets := []any{&p.ID, &p.InstrumentID, &p.IsLong, &p.UseDebt, &p.EntryPriceRaw, &p.MarginRaw, &p.Leverage, &p.BustPriceRaw, &p.OpenedAtMs, &p.IntentID}
	for i, dst := range targets {
		if err := json.Unmarshal(t[i], dst); err != nil {
			return fmt.Errorf("paper position field %d: %w", i, err)
		}
	}
	return nil
}

// Wallet is the account snapshot pushed by /state/user/live. Raw amounts are
// 18-decimal integers.
type Wallet struct {
	Address             string     `json:"address"`
	IndexedThroughBlock int64      `json:"indexedThroughBlock"`
	SessionKeys         [][]string `json:"sessionKeys"` // [key, expiry seconds]
	Balance             string     `json:"balance"`
	Queued              string     `json:"queued"`
	Funds               struct {
		AvailableBalance string `json:"availableBalance"`
	} `json:"funds"`
	Paper struct {
		Balance     string `json:"balance"`
		Staked      string `json:"staked"`
		PendingBase string `json:"pendingBase"`
	} `json:"paper"`
	OpenPositions []Position `json:"openPositions"`
}

// HasSessionKey reports whether key is registered and unexpired at now.
func (w Wallet) HasSessionKey(key common.Address, now time.Time) bool {
	for _, k := range w.SessionKeys {
		if len(k) < 2 || !strings.EqualFold(k[0], key.Hex()) {
			continue
		}
		var exp int64
		if _, err := fmt.Sscan(k[1], &exp); err == nil && exp > now.Unix() {
			return true
		}
	}
	return false
}

// Wallet reads one snapshot from the account's live stream and disconnects.
func (c *Client) Wallet(ctx context.Context, user common.Address) (Wallet, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := c.request(ctx, http.MethodGet, "/state/user/live?wallet="+wireAddress(user), nil)
	if err != nil {
		return Wallet{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return Wallet{}, fmt.Errorf("paper wallet stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Wallet{}, fmt.Errorf("paper wallet stream: HTTP %d: %s", resp.StatusCode, raw)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:") && event == "wallet":
			var w Wallet
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &w); err != nil {
				return Wallet{}, fmt.Errorf("paper wallet event: %w", err)
			}
			if !strings.EqualFold(w.Address, user.Hex()) {
				return Wallet{}, fmt.Errorf("paper wallet event for %s, asked for %s", w.Address, user.Hex())
			}
			return w, nil
		case line == "":
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		return Wallet{}, fmt.Errorf("paper wallet stream: %w", err)
	}
	return Wallet{}, errors.New("paper wallet stream ended without a wallet event")
}

// Trade is one settled close or liquidation from the account history.
type Trade struct {
	TimestampMs       int64  `json:"timestampMs"`
	TxHash            string `json:"txHash"`
	PositionID        string `json:"positionId"`
	InstrumentID      uint32 `json:"instrumentId"`
	Symbol            string `json:"symbol"`
	Kind              string `json:"kind"`
	IsLong            bool   `json:"isLong"`
	EntryPriceRaw     string `json:"entryPriceRaw"`
	ExitPriceRaw      string `json:"exitPriceRaw"`
	MarginRaw         string `json:"marginRaw"`
	Leverage          int    `json:"leverage"`
	AdjustedPnlRaw    string `json:"adjustedPnlRaw"`
	UserPaidFeeRaw    string `json:"userPaidFeeRaw"`
	PaperMintedRaw    string `json:"paperMintedRaw"`
	PaperMintBasisRaw string `json:"paperMintBasisRaw"`
}

// TradesPage is the only page size the trades route accepts; any other
// limit is rejected with HTTP 400 (seen live 2026-10-11).
const TradesPage = 75

// Trades returns up to limit of the most recent closes, newest first;
// limit <= 0 or above TradesPage returns one full page.
func (c *Client) Trades(ctx context.Context, user common.Address, limit int) ([]Trade, error) {
	var res struct {
		Items []Trade `json:"items"`
	}
	if err := c.get(ctx, fmt.Sprintf("/query/accounts/%s/trades?limit=%d", wireAddress(user), TradesPage), &res); err != nil {
		return nil, err
	}
	if limit > 0 && limit < len(res.Items) {
		res.Items = res.Items[:limit]
	}
	return res.Items, nil
}

// MarketParams are the per-instrument limits and impact parameters.
type MarketParams struct {
	ID          uint32
	Symbol      string
	PriceScale  int64
	MaxLeverage int
	MaxNotional decimal.Decimal
	Impact      ImpactParams
}

// TradingState is /state/trading: pause flag, minimums and markets.
type TradingState struct {
	Paused          bool
	MinimumMargin   decimal.Decimal
	MinimumNotional decimal.Decimal
	Markets         []MarketParams
}

func (s TradingState) Market(symbol string) (MarketParams, error) {
	for _, m := range s.Markets {
		if m.Symbol == symbol {
			return m, nil
		}
	}
	return MarketParams{}, fmt.Errorf("paper market %s not listed", symbol)
}

func (c *Client) Trading(ctx context.Context) (TradingState, error) {
	var raw struct {
		TradingPaused          bool                `json:"tradingPaused"`
		MinimumTradeSizeRaw    string              `json:"minimumTradeSizeRaw"`
		MinimumOpenNotionalRaw string              `json:"minimumOpenNotionalRaw"`
		Instruments            [][]json.RawMessage `json:"instruments"`
	}
	if err := c.get(ctx, "/state/trading", &raw); err != nil {
		return TradingState{}, err
	}
	s := TradingState{Paused: raw.TradingPaused}
	var err error
	if s.MinimumMargin, err = FromRaw(raw.MinimumTradeSizeRaw); err != nil {
		return TradingState{}, err
	}
	if s.MinimumNotional, err = FromRaw(raw.MinimumOpenNotionalRaw); err != nil {
		return TradingState{}, err
	}
	for _, t := range raw.Instruments {
		m, err := parseMarket(t)
		if err != nil {
			return TradingState{}, err
		}
		s.Markets = append(s.Markets, m)
	}
	return s, nil
}

// parseMarket reads the instrument tuple. Indexes match the on-chain
// instruments() struct: 12 rateMultiplier, 13 positionMultiplier,
// 14 maxLeverage, 15 maxNotional, 17 baseRate, 18 referenceNotional.
func parseMarket(t []json.RawMessage) (MarketParams, error) {
	if len(t) < 19 {
		return MarketParams{}, fmt.Errorf("paper instrument tuple has %d fields, want >= 19", len(t))
	}
	var m MarketParams
	var scale string
	var rateMult, posMult, maxNotional, baseRate, refNotional string
	targets := map[int]any{0: &m.ID, 1: &m.Symbol, 3: &scale, 12: &rateMult, 13: &posMult, 14: &m.MaxLeverage, 15: &maxNotional, 17: &baseRate, 18: &refNotional}
	for i, dst := range targets {
		if err := json.Unmarshal(t[i], dst); err != nil {
			return MarketParams{}, fmt.Errorf("paper instrument field %d: %w", i, err)
		}
	}
	if _, err := fmt.Sscan(scale, &m.PriceScale); err != nil || m.PriceScale <= 0 {
		return MarketParams{}, fmt.Errorf("paper instrument %s price scale %q", m.Symbol, scale)
	}
	vals := map[string]decimal.Decimal{}
	for name, raw := range map[string]string{"rate": rateMult, "pos": posMult, "notional": maxNotional, "base": baseRate, "ref": refNotional} {
		v, err := FromRaw(raw)
		if err != nil {
			return MarketParams{}, err
		}
		vals[name] = v
	}
	m.MaxNotional = vals["notional"]
	m.Impact = ImpactParams{
		BaseRate:           vals["base"].InexactFloat64(),
		RateMultiplier:     vals["rate"].InexactFloat64(),
		PositionMultiplier: vals["pos"].InexactFloat64(),
		ReferenceNotional:  vals["ref"].InexactFloat64(),
	}
	return m, nil
}

// Summary is the part of /query/protocol/summary the mint curve needs.
type Summary struct {
	TrackedLP    decimal.Decimal
	TailProgress decimal.Decimal
	FlatRate     decimal.Decimal
	DecayScale   decimal.Decimal
	QueueBalance decimal.Decimal
	PaperSupply  decimal.Decimal
	PaperStaked  decimal.Decimal
	LifetimeFees decimal.Decimal
}

// MintRate is the PAPER minted per dollar of loss basis right now.
func (s Summary) MintRate() float64 {
	return MintRate(s.FlatRate.InexactFloat64(), s.DecayScale.InexactFloat64(), s.TailProgress.InexactFloat64())
}

func (c *Client) Summary(ctx context.Context) (Summary, error) {
	var raw struct {
		Balances struct {
			Queue string `json:"queue"`
		} `json:"balances"`
		Fees struct {
			Lifetime string `json:"lifetime"`
		} `json:"fees"`
		Paper struct {
			Supply       string `json:"supply"`
			Staked       string `json:"staked"`
			TrackedLp    string `json:"trackedLp"`
			TailProgress string `json:"tailProgress"`
			Rate         string `json:"rate"`
			Decay        string `json:"decay"`
		} `json:"paper"`
	}
	if err := c.get(ctx, "/query/protocol/summary", &raw); err != nil {
		return Summary{}, err
	}
	var s Summary
	for _, f := range []struct {
		dst *decimal.Decimal
		raw string
	}{
		{&s.QueueBalance, raw.Balances.Queue}, {&s.LifetimeFees, raw.Fees.Lifetime},
		{&s.PaperSupply, raw.Paper.Supply}, {&s.PaperStaked, raw.Paper.Staked},
		{&s.TrackedLP, raw.Paper.TrackedLp}, {&s.TailProgress, raw.Paper.TailProgress},
		{&s.FlatRate, raw.Paper.Rate}, {&s.DecayScale, raw.Paper.Decay},
	} {
		v, err := FromRaw(f.raw)
		if err != nil {
			return Summary{}, err
		}
		*f.dst = v
	}
	if s.FlatRate.IsZero() || s.DecayScale.IsZero() {
		return Summary{}, errors.New("paper summary is missing the mint curve parameters")
	}
	return s, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	status, err := c.do(ctx, http.MethodGet, path, nil, out)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("paper %s: HTTP %d", path, status)
	}
	return nil
}

// do returns the HTTP status even when decoding fails, so callers can report
// relayer rejections that carry a JSON error body.
func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("paper %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("paper %s: HTTP %d: decode: %w: %.512s", path, resp.StatusCode, err, raw)
	}
	return resp.StatusCode, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Papertrade-Build", c.build)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
