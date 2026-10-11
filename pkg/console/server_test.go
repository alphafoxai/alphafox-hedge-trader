package console

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/farm"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hyperliquid"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
)

// Shapes trimmed from the live relayer (2026-10-11); ETH cloned from BTC
// with its own scale and multiplier.
const (
	tradingBody = `{"tradingPaused":false,"minimumTradeSizeRaw":"10000000000000000000","minimumOpenNotionalRaw":"10000000000000000000000","instruments":[` +
		`[0,"BTC",0,"10",true,false,false,false,false,null,null,null,"15000000000000000000000","814598000000000000000",1000,"10000000000000000000000000","0","100000000000000000","100000000000000000000000","0","0","0","0"],` +
		`[1,"ETH",0,"100",true,false,false,false,false,null,null,null,"15000000000000000000000","483979000000000000000",1000,"10000000000000000000000000","0","100000000000000000","100000000000000000000000","0","0","0","0"]]}`
	summaryBody = `{"ok":true,"balances":{"queue":"0"},"fees":{"lifetime":"0"},"paper":{"supply":"0","staked":"0","trackedLp":"0","tailProgress":"77991670838876812565013732","rate":"100000000000000000000","decay":"120000000000000000000000000"}}`
)

type env struct {
	t       *testing.T
	srv     *Server
	http    *httptest.Server
	master  *evmsign.LocalSigner
	session *evmsign.LocalSigner
	keyHex  string
	posts   int // relayer writes
	mu      sync.Mutex
}

func newKey(t *testing.T) (*evmsign.LocalSigner, string) {
	k, _ := crypto.GenerateKey()
	h := "0x" + hex.EncodeToString(crypto.FromECDSA(k))
	s, err := evmsign.NewLocalSigner(h)
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}

func newEnv(t *testing.T, live bool) *env {
	e := &env{t: t}
	e.master, _ = newKey(t)
	e.session, e.keyHex = newKey(t)
	relayer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			e.mu.Lock()
			e.posts++
			e.mu.Unlock()
			t.Errorf("relayer write %s %s", r.Method, r.URL)
		case r.URL.Path == "/state/trading":
			io.WriteString(w, tradingBody)
		case r.URL.Path == "/query/protocol/summary":
			io.WriteString(w, summaryBody)
		case r.URL.Path == "/state/user/live":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: wallet\ndata: {\"address\":%q,\"sessionKeys\":[[%q,\"%d\"]],\"balance\":\"100000000000000000000\",\"funds\":{\"availableBalance\":\"30000000000000000000\"},\"paper\":{\"balance\":\"0\",\"staked\":\"0\",\"pendingBase\":\"0\"},\"openPositions\":[]}\n\n",
				strings.ToLower(e.master.Address().Hex()), strings.ToLower(e.session.Address().Hex()), time.Now().Add(48*time.Hour).Unix())
		case strings.HasSuffix(r.URL.Path, "/trades"):
			io.WriteString(w, `{"items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(relayer.Close)
	hl := httptest.NewServer(http.HandlerFunc(hyperliquidInfo))
	t.Cleanup(hl.Close)
	srv, err := New(paper.NewClient(relayer.URL, "", nil), hyperliquid.NewInfoClientWithBaseURL(hl.URL, nil), live)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	e.http = httptest.NewServer(srv.Handler())
	t.Cleanup(e.http.Close)
	return e
}

// hyperliquidInfo serves l2Book and a candle series whose 1m log returns
// alternate ±0.1%, i.e. σ = 0.1%·√60 per √hour.
func hyperliquidInfo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
		Req  struct {
			StartTime, EndTime int64
		} `json:"req"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	switch body.Type {
	case "l2Book":
		io.WriteString(w, `{"levels":[[{"px":"100","sz":"1","n":1}],[{"px":"100.02","sz":"1","n":1}]]}`)
	case "candleSnapshot":
		var cs []map[string]any
		px := 100.0
		for ts, i := body.Req.StartTime, 0; ts < body.Req.EndTime; ts, i = ts+60_000, i+1 {
			if i%2 == 0 {
				px *= math.Exp(0.001)
			} else {
				px *= math.Exp(-0.001)
			}
			c := fmt.Sprintf("%.8f", px)
			cs = append(cs, map[string]any{"t": ts, "T": ts + 59_999, "o": c, "h": c, "l": c, "c": c, "i": "1m", "n": 1})
		}
		json.NewEncoder(w).Encode(cs)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (e *env) call(method, path string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = strings.NewReader(string(raw))
	}
	req, _ := http.NewRequest(method, e.http.URL+path, rd)
	req.Header.Set("X-Console-Token", e.srv.Token())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, raw)
		}
	}
	return res.StatusCode
}

func (e *env) creds() map[string]any {
	return map[string]any{"account": e.master.Address().Hex(), "sessionKey": e.keyHex}
}

func TestGuards(t *testing.T) {
	e := newEnv(t, false)
	get := func(host, token string) int {
		req, _ := http.NewRequest("GET", e.http.URL+"/api/config", nil)
		if host != "" {
			req.Host = host
		}
		if token != "" {
			req.Header.Set("X-Console-Token", token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if c := get("", e.srv.Token()); c != 200 {
		t.Fatalf("good request %d", c)
	}
	if c := get("", ""); c != http.StatusForbidden {
		t.Errorf("no token %d", c)
	}
	if c := get("", "wrong"); c != http.StatusForbidden {
		t.Errorf("wrong token %d", c)
	}
	if c := get("evil.example:8547", e.srv.Token()); c != http.StatusForbidden {
		t.Errorf("rebinding host %d", c)
	}
	res, err := http.Get(e.http.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), e.srv.Token()) || !strings.Contains(string(page), `content="false"`) {
		t.Error("page lacks token or mode")
	}
	for _, a := range []string{"/assets/app.js", "/assets/app.css"} {
		res, err := http.Get(e.http.URL + a)
		if err != nil || res.StatusCode != 200 {
			t.Errorf("%s: %v %v", a, res.StatusCode, err)
		}
		res.Body.Close()
	}
}

func TestMarketRecommends(t *testing.T) {
	e := newEnv(t, false)
	var out struct {
		Paused   bool
		MintRate float64
		Window   string
		Markets  []marketView
	}
	if c := e.call("GET", "/api/market?capital=400&hours=2&window=1h", nil, &out); c != 200 {
		t.Fatalf("status %d", c)
	}
	if out.Window != "1h0m0s" || len(out.Markets) != 2 || out.MintRate <= 0 {
		t.Fatalf("%+v", out)
	}
	want := 0.001 * math.Sqrt(60)
	for _, m := range out.Markets {
		if m.Error != "" {
			t.Fatalf("%s: %s", m.Symbol, m.Error)
		}
		if math.Abs(m.Sigma-want) > 1e-6 || math.Abs(m.SigmaDay-want) > 1e-6 {
			t.Errorf("%s σ %v / %v, want %v", m.Symbol, m.Sigma, m.SigmaDay, want)
		}
		if m.Mid != 100.01 || m.MaxLeverage != 1000 {
			t.Errorf("%s mid %v max %d", m.Symbol, m.Mid, m.MaxLeverage)
		}
		p := m.Plan
		if !p.Feasible || p.Margin*2 > 200+1e-9 || p.CloseMove+p.BustGuard > p.BustDistance+1e-12 {
			t.Errorf("%s plan %+v", m.Symbol, p)
		}
	}
	// Without capital there is volatility but no plan.
	e.call("GET", "/api/market", nil, &out)
	if out.Markets[0].Plan.Feasible || out.Markets[0].Sigma == 0 {
		t.Errorf("no-capital market %+v", out.Markets[0])
	}
}

func TestSessionRegistrationDryRun(t *testing.T) {
	e := newEnv(t, false)
	var key struct{ Address, PrivateKey string }
	e.call("POST", "/api/session/new", struct{}{}, &key)
	var addr struct{ Address string }
	e.call("POST", "/api/session/address", map[string]string{"sessionKey": key.PrivateKey}, &addr)
	if addr.Address != key.Address || !common.IsHexAddress(key.Address) {
		t.Fatalf("new %+v, derived %+v", key, addr)
	}
	var td apitypes.TypedData
	if c := e.call("POST", "/api/session/typed-data", map[string]any{"account": e.master.Address().Hex(), "sessionKey": key.Address, "days": 7}, &td); c != 200 {
		t.Fatalf("typed data %d", c)
	}
	if td.PrimaryType != "RegisterSessionKey" {
		t.Fatalf("%+v", td)
	}
	sig, err := e.master.SignTypedData(td) // the browser wallet's job
	if err != nil {
		t.Fatal(err)
	}
	raw := "0x" + strings.TrimPrefix(sig.R, "0x") + strings.TrimPrefix(sig.S, "0x") + hex.EncodeToString([]byte{byte(sig.V)})
	var res struct {
		IntentID string
		Result   paper.SubmitResult
	}
	if c := e.call("POST", "/api/session/register", map[string]any{"account": e.master.Address().Hex(), "typedData": td, "signature": raw}, &res); c != 200 || !res.Result.DryRun {
		t.Fatalf("register %d %+v", c, res)
	}
	// A signature from anyone but the account is refused.
	other, _ := newKey(t)
	sig, _ = other.SignTypedData(td)
	raw = "0x" + strings.TrimPrefix(sig.R, "0x") + strings.TrimPrefix(sig.S, "0x") + hex.EncodeToString([]byte{byte(sig.V)})
	if c := e.call("POST", "/api/session/register", map[string]any{"account": e.master.Address().Hex(), "typedData": td, "signature": raw}, nil); c != 400 {
		t.Errorf("foreign signature %d", c)
	}
}

func TestDryRunStartPreviewsOnly(t *testing.T) {
	e := newEnv(t, false)
	req := e.creds()
	for k, v := range map[string]any{"symbol": "ETH", "margin": "10", "leverage": 1000, "closeMove": "0.0003", "bustGuard": "0.0001", "cycles": 1} {
		req[k] = v
	}
	var out struct {
		DryRun      bool
		OpenIntents []paper.Intent
	}
	if c := e.call("POST", "/api/runs", req, &out); c != 200 || !out.DryRun || len(out.OpenIntents) != 2 {
		t.Fatalf("%d %+v", c, out)
	}
	var runs []runJSON
	e.call("GET", "/api/runs", nil, &runs)
	if len(runs) != 0 {
		t.Errorf("dry run started %d runs", len(runs))
	}
	// The master key is not a session key.
	_, masterHex := newKey(t)
	bad := map[string]any{"account": "", "sessionKey": masterHex}
	m, _ := evmsign.NewLocalSigner(masterHex)
	bad["account"] = m.Address().Hex()
	if c := e.call("POST", "/api/claim", bad, nil); c != 400 {
		t.Errorf("master key accepted: %d", c)
	}
}

// runJSON is the wire form of a Run (which holds a lock).
type runJSON struct {
	Symbol   string
	State    string
	Config   RunRequest
	Results  []farm.Result
	OpenPair []string
	Logs     []string
	NetCost  string
	Paper    string `json:"paperMinted"`
	Error    string
}

// fakeExchange fills opens at 100 and settles closes; positions never move
// unless the test's price does.
type fakeExchange struct {
	mu      sync.Mutex
	next    int
	open    []farm.Position
	closed  [][]string
	staked  []*big.Int
	settled map[string]farm.Settled
}

func (f *fakeExchange) Open(_ context.Context, sym string, isLong bool, margin decimal.Decimal, lev int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprint(f.next)
	bust := decimal.NewFromFloat(99.95)
	if !isLong {
		bust = decimal.NewFromFloat(100.05)
	}
	f.open = append(f.open, farm.Position{ID: id, IntentID: "i" + id, Symbol: sym, IsLong: isLong,
		Entry: decimal.NewFromInt(100), Bust: bust, Margin: margin, Leverage: lev})
	return "i" + id, nil
}

func (f *fakeExchange) Close(_ context.Context, ids []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, ids)
	keep := f.open[:0]
	for _, p := range f.open {
		gone := false
		for _, id := range ids {
			gone = gone || p.ID == id
		}
		if !gone {
			keep = append(keep, p)
			continue
		}
		pnl := decimal.NewFromFloat(-3)
		if p.IsLong {
			pnl = decimal.NewFromFloat(1)
		}
		f.settled[p.ID] = farm.Settled{PositionID: p.ID, IsLong: p.IsLong, PnL: pnl, PaperMinted: decimal.NewFromInt(500)}
	}
	f.open = keep
	return "close", nil
}

func (f *fakeExchange) Positions(context.Context) ([]farm.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]farm.Position(nil), f.open...), nil
}

func (f *fakeExchange) Settled(_ context.Context, ids []string) ([]farm.Settled, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []farm.Settled
	for _, id := range ids {
		if s, ok := f.settled[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeExchange) Stake(_ context.Context, amt *big.Int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staked = append(f.staked, amt)
	return "stake", nil
}

func liveEnv(t *testing.T, price *decimal.Decimal, mu *sync.Mutex) (*env, *fakeExchange) {
	e := newEnv(t, true)
	ex := &fakeExchange{settled: map[string]farm.Settled{}}
	e.srv.NewExchange = func(common.Address, evmsign.Signer) Exchange { return ex }
	e.srv.Price = func(context.Context, string) (decimal.Decimal, error) {
		mu.Lock()
		defer mu.Unlock()
		return *price, nil
	}
	e.srv.FarmOptions = []farm.Option{farm.WithClock(time.Now, func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
			return nil
		}
	})}
	return e, ex
}

func (e *env) runRequest(sym string, cycles int, stake bool) map[string]any {
	req := e.creds()
	for k, v := range map[string]any{"symbol": sym, "margin": "10", "leverage": 1000, "closeMove": "0.0003",
		"bustGuard": "0.0001", "cycles": cycles, "stakeEachCycle": stake} {
		req[k] = v
	}
	return req
}

func (e *env) waitRun(sym string, done func(runJSON) bool) runJSON {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var runs []runJSON
		e.call("GET", "/api/runs", nil, &runs)
		for _, r := range runs {
			if r.Symbol == sym && done(r) {
				return r
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s run never reached the expected state: %+v", sym, runs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunCompletesAndStakes(t *testing.T) {
	var mu sync.Mutex
	px := decimal.RequireFromString("100.04") // past the 0.03% trigger
	e, ex := liveEnv(t, &px, &mu)
	var started runJSON
	if c := e.call("POST", "/api/runs", e.runRequest("BTC", 2, true), &started); c != 200 || started.State != StateRunning {
		t.Fatalf("%d %+v", c, started)
	}
	r := e.waitRun("BTC", func(r runJSON) bool { return r.State == StateDone })
	if len(r.Results) != 2 || r.NetCost != "4.0000" || r.Paper != "2000.00" || r.Config.SessionKey != "" {
		t.Fatalf("%+v", r)
	}
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if len(ex.closed) != 2 || len(ex.staked) != 2 || ex.staked[0].Cmp(paper.ToRaw(decimal.NewFromInt(1000))) != 0 {
		t.Errorf("closes %v stakes %v", ex.closed, ex.staked)
	}
	if e.posts != 0 {
		t.Errorf("%d relayer writes", e.posts)
	}
}

func TestRunStopLeavesOrClosesPair(t *testing.T) {
	for _, closeIt := range []bool{false, true} {
		t.Run(fmt.Sprint("close=", closeIt), func(t *testing.T) {
			var mu sync.Mutex
			px := decimal.NewFromInt(100) // never triggers
			e, ex := liveEnv(t, &px, &mu)
			if c := e.call("POST", "/api/runs", e.runRequest("ETH", 3, false), nil); c != 200 {
				t.Fatalf("start %d", c)
			}
			e.waitRun("ETH", func(r runJSON) bool {
				for _, l := range r.Logs {
					if strings.Contains(l, "opened long") {
						return true
					}
				}
				return false
			})
			// One symbol runs at a time; a second ETH run is refused.
			if c := e.call("POST", "/api/runs", e.runRequest("ETH", 1, false), nil); c != 400 {
				t.Errorf("second run %d", c)
			}
			// Manual closes wait for the run.
			if c := e.call("POST", "/api/close", map[string]any{"account": e.master.Address().Hex(), "sessionKey": e.keyHex, "ids": []string{"1"}}, nil); c != 400 {
				t.Errorf("manual close during run %d", c)
			}
			path := "/api/runs/ETH/stop"
			if closeIt {
				path += "?close=1"
			}
			var r runJSON
			e.call("POST", path, struct{}{}, &r)
			if r.State != StateStopped {
				t.Fatalf("%+v", r)
			}
			ex.mu.Lock()
			defer ex.mu.Unlock()
			if closeIt {
				if len(ex.closed) != 1 || len(ex.open) != 0 || len(r.OpenPair) != 0 {
					t.Errorf("closes %v open %v pair %v", ex.closed, ex.open, r.OpenPair)
				}
			} else {
				if len(ex.closed) != 0 || len(r.OpenPair) != 2 || len(e.srv.OpenPairs()["ETH"]) != 2 {
					t.Errorf("closes %v pair %v", ex.closed, r.OpenPair)
				}
			}
		})
	}
}

func TestRunNeedsBalanceForBothSymbols(t *testing.T) {
	var mu sync.Mutex
	px := decimal.NewFromInt(100)
	e, _ := liveEnv(t, &px, &mu)
	// 30 available: one pair of 10+10 fits, a second does not.
	if c := e.call("POST", "/api/runs", e.runRequest("BTC", 1, false), nil); c != 200 {
		t.Fatalf("first %d", c)
	}
	var out map[string]string
	if c := e.call("POST", "/api/runs", e.runRequest("ETH", 1, false), &out); c != 400 || !strings.Contains(out["error"], "available balance") {
		t.Errorf("second %d %v", c, out)
	}
	e.srv.Stop(true)
}
