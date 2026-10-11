package paper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

func TestSubmitPostsIntent(t *testing.T) {
	b, _ := newTestIntents(t)
	in, err := b.Open(Instrument{ID: 1, Symbol: "ETH", PriceScale: 100}, true, big.NewInt(1e18), 50)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/intents" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Papertrade-Build") != "build-x" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		var body struct {
			Action string         `json:"action"`
			Signed map[string]any `json:"signed"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil || body.Action != "openPosition" || body.Signed["leverage"] != "50" {
			t.Errorf("body %s", raw)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"intentId": in.ID.Hex(), "action": "openPosition", "state": "queued", "queueRevision": "33"})
	}))
	defer srv.Close()

	res, err := NewClient(srv.URL, "build-x", nil).Submit(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != "queued" || res.IntentID != in.ID.Hex() {
		t.Fatalf("result %+v", res)
	}
}

func TestSubmitRejectsAndChecksID(t *testing.T) {
	b, _ := newTestIntents(t)
	in, _ := b.Close([]string{"1"})
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"rejected":    {422, `{"ok":false,"error":"session_key_unknown","message":"Authenticate trading first."}`, "session_key_unknown"},
		"error field": {200, `{"error":"intake_action_paused","message":"paused"}`, "intake_action_paused"},
		"id drift":    {200, `{"intentId":"0x0000000000000000000000000000000000000000000000000000000000000001","state":"queued"}`, "differs from local"},
		"not json":    {502, `bad gateway`, "HTTP 502"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "", nil).Submit(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
	var re *RelayerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		io.WriteString(w, `{"ok":false,"code":"nonce_used","message":"Nonce already used"}`)
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "", nil).Submit(context.Background(), in); !errors.As(err, &re) || re.Code != "nonce_used" || re.Status != 409 {
		t.Fatalf("err %v, want RelayerError nonce_used", err)
	}
}

func TestSubmitDryRunSendsNothing(t *testing.T) {
	b, _ := newTestIntents(t)
	in, _ := b.Claim()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("dry run sent %s %s", r.Method, r.URL)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", nil)
	c.DryRun = true
	res, err := c.Submit(context.Background(), in)
	if err != nil || !res.DryRun || res.IntentID != in.ID.Hex() {
		t.Fatalf("dry run %+v, %v", res, err)
	}
}

// Trimmed from a live /state/user/live stream (2026-10-11).
const liveStream = `: connected

event: liveness
data: {"heartbeatIntervalMs":10000}

event: wallet
data: {"address":"0xacadbd12c0eb153bb3e56f337eb34850325a6cec","indexedThroughBlock":48239391,"sessionKeys":[["0x975c4e3deb47f1661565cae170a8426deba53946","1794130920"]],"balance":"122948781715220863516","queued":"0","funds":{"availableBalance":"122948781715220863516"},"paper":{"balance":"0","staked":"9373958184474861154996767","pendingBase":"2568771900586527323544"},"openPositions":[["134819",0,false,false,"829695","6085964756000000000000",200,"833447",1791705247000,"0x2dbe19b3b6d8d9f5b3129f49b30c633ca9d97c91f852598e081a777a51827eba"],["134815",0,true,false,"829695","6085964756000000000000",200,"825939",1791705246000,"0xd403583e17989b20032eddfadb0c73e5ebcb24bade13ba2cd7c11b2911487b90"]]}

event: heartbeat
data: null

`

var liveUser = common.HexToAddress("0xacadbd12c0eb153bb3e56f337eb34850325a6cec")

func TestWalletReadsFirstWalletEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/state/user/live" || r.URL.Query().Get("wallet") != "0xacadbd12c0eb153bb3e56f337eb34850325a6cec" {
			t.Errorf("url %s", r.URL)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, liveStream)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a live stream never ends on its own
	}))
	defer srv.Close()
	w, err := NewClient(srv.URL, "", nil).Wallet(context.Background(), liveUser)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.OpenPositions) != 2 {
		t.Fatalf("positions %+v", w.OpenPositions)
	}
	p := w.OpenPositions[1]
	if p.ID != "134815" || !p.IsLong || p.Leverage != 200 || p.BustPriceRaw != "825939" ||
		p.IntentID != "0xd403583e17989b20032eddfadb0c73e5ebcb24bade13ba2cd7c11b2911487b90" {
		t.Fatalf("position %+v", p)
	}
	key := common.HexToAddress("0x975c4e3deb47f1661565cae170a8426deba53946")
	seen := time.UnixMilli(1791705247000)
	if !w.HasSessionKey(key, seen) || w.HasSessionKey(key, seen.AddDate(0, 2, 0)) {
		t.Fatal("session key expiry not honoured")
	}

	acct := &Account{Client: NewClient(srv.URL, "", nil), Intents: NewIntents(liveUser, nil, nil, 0, nil)}
	ps, err := acct.Positions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ps[0].Entry.Equal(decimal.RequireFromString("82969.5")) || !ps[0].Bust.Equal(decimal.RequireFromString("83344.7")) ||
		!ps[0].Margin.Equal(decimal.RequireFromString("6085.964756")) || ps[0].Symbol != "BTC" || ps[0].IsLong {
		t.Fatalf("farm position %+v", ps[0])
	}
}

// Trimmed from /query/accounts/{addr}/trades: the two legs of one pair.
const tradesBody = `{"items":[
{"timestampMs":1791705205000,"txHash":"0x1f91","positionId":"131659","instrumentId":0,"symbol":"BTC","isLong":false,"entryPriceRaw":"830725","marginRaw":"6128830578000000000000","leverage":200,"kind":"PositionClosed","exitPriceRaw":"829695","adjustedPnlRaw":"1165675949880586831779","userPaidFeeRaw":"23313518997611736635","paperMintedRaw":"0","paperMintBasisRaw":"0"},
{"timestampMs":1791705205000,"txHash":"0x1f91","positionId":"131656","instrumentId":0,"symbol":"BTC","isLong":true,"entryPriceRaw":"830725","marginRaw":"6128830578000000000000","leverage":200,"kind":"PositionClosed","exitPriceRaw":"829695","adjustedPnlRaw":"-1519803905104577327033","userPaidFeeRaw":"0","paperMintedRaw":"54709706581921589294835","paperMintBasisRaw":"1489407827002485780493"},
{"positionId":"1","instrumentId":0,"kind":"PositionClosed","exitPriceRaw":"1","adjustedPnlRaw":"0","userPaidFeeRaw":"0","paperMintedRaw":"0"}]}`

func TestSettledFromTrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query/accounts/0xacadbd12c0eb153bb3e56f337eb34850325a6cec/trades" || r.URL.Query().Get("limit") != "75" {
			t.Errorf("url %s", r.URL)
		}
		io.WriteString(w, tradesBody)
	}))
	defer srv.Close()
	acct := &Account{Client: NewClient(srv.URL, "", nil), Intents: NewIntents(liveUser, nil, nil, 0, nil)}
	got, err := acct.Settled(context.Background(), []string{"131656", "131659"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("settled %+v", got)
	}
	if two, err := acct.Client.Trades(context.Background(), liveUser, 2); err != nil || len(two) != 2 {
		t.Fatalf("Trades(limit 2) = %d rows, %v; must still request limit=75", len(two), err)
	}
	short, long := got[0], got[1]
	if !short.PnL.Equal(decimal.RequireFromString("1142.362430882975095144")) || !short.Exit.Equal(decimal.RequireFromString("82969.5")) {
		t.Fatalf("winner %+v", short)
	}
	if !long.PnL.Equal(decimal.RequireFromString("-1519.803905104577327033")) || !long.PaperMinted.Equal(decimal.RequireFromString("54709.706581921589294835")) {
		t.Fatalf("loser %+v", long)
	}
}

// Trimmed from /state/trading and /query/protocol/summary (2026-10-11).
const tradingBody = `{"schemaVersion":1,"tradingPaused":false,"minimumTradeSizeRaw":"10000000000000000000","minimumOpenNotionalRaw":"10000000000000000000000","instruments":[[0,"BTC",0,"10",true,false,false,false,false,null,null,null,"15000000000000000000000","814598000000000000000",1000,"10000000000000000000000000","0","100000000000000000","100000000000000000000000","0","0","0","0"]]}`

const summaryBody = `{"ok":true,"balances":{"queue":"0"},"fees":{"lifetime":"24661347673766625000035685"},"paper":{"supply":"5383950624703571269398738824","staked":"5321771190712822855665404447","trackedLp":"5002858346463989506014629","tailProgress":"77991670838876812565013732","rate":"100000000000000000000","decay":"120000000000000000000000000"}}`

func TestTradingAndSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/state/trading":
			io.WriteString(w, tradingBody)
		case "/query/protocol/summary":
			io.WriteString(w, summaryBody)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "", nil)
	ts, err := c.Trading(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m, err := ts.Market("BTC")
	if err != nil {
		t.Fatal(err)
	}
	if !ts.MinimumMargin.Equal(decimal.NewFromInt(10)) || !ts.MinimumNotional.Equal(decimal.NewFromInt(10_000)) ||
		m.MaxLeverage != 1000 || m.PriceScale != 10 || m.Impact != LaunchImpact["BTC"] {
		t.Fatalf("trading %+v market %+v", ts, m)
	}
	s, err := c.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	near(t, "mint rate", s.MintRate(), 36.73, 0.01)
}
