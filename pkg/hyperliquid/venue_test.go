package hyperliquid

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

const testAgentKey = "0x0123456789012345678901234567890123456789012345678901234567890123"

type fakeVenueServer struct {
	t        *testing.T
	exchange []map[string]json.RawMessage
}

func (f *fakeVenueServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Fatalf("bad request body: %s", raw)
	}
	if r.URL.Path == "/exchange" {
		f.exchange = append(f.exchange, body)
		var action struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(body["action"], &action)
		switch action.Type {
		case "order":
			io.WriteString(w, `{"status":"ok","response":{"type":"order","data":{"statuses":[{"filled":{"totalSz":"0.01","avgPx":"60010.0","oid":77}}]}}}`)
		case "cancel":
			io.WriteString(w, `{"status":"ok","response":{"type":"cancel","data":{"statuses":[{"error":"Order was never placed, already canceled, or filled."}]}}}`)
		default:
			io.WriteString(w, `{"status":"err","response":"unsupported"}`)
		}
		return
	}
	var kind string
	_ = json.Unmarshal(body["type"], &kind)
	switch kind {
	case "meta":
		io.WriteString(w, `{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":40},{"name":"ETH","szDecimals":4,"maxLeverage":25}]}`)
	case "l2Book":
		io.WriteString(w, `{"coin":"BTC","time":1760000000000,"levels":[[{"px":"60000.0","sz":"1.2","n":3}],[{"px":"60001.0","sz":"0.8","n":2}]]}`)
	case "clearinghouseState":
		io.WriteString(w, `{"assetPositions":[{"position":{"coin":"BTC","szi":"-0.01","entryPx":"60010.0","unrealizedPnl":"0.1","leverage":{"type":"cross","value":10}}}],"marginSummary":{"accountValue":"100"},"withdrawable":"50"}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func newTestVenue(t *testing.T) (*Venue, *fakeVenueServer, *evmsign.LocalSigner) {
	t.Helper()
	fake := &fakeVenueServer{t: t}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	signer, err := evmsign.NewLocalSigner(testAgentKey)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVenue(VenueConfig{
		AccountAddress:  "0x1111111111111111111111111111111111111111",
		Builder:         &BuilderWire{B: "0xF945417968B53ADC1E0489E5A287D2E41C8B3D79", F: 50},
		DefaultSlippage: decimal.RequireFromString("0.01"),
	}, NewInfoClientWithBaseURL(srv.URL, nil), NewExchangeClientWithBaseURL(srv.URL, Mainnet, signer, nil))
	if err != nil {
		t.Fatal(err)
	}
	return v, fake, signer
}

func TestMarketOrderIsSignedIOCAcrossTheBook(t *testing.T) {
	v, fake, signer := newTestVenue(t)
	res, err := v.PlaceOrder(context.Background(), venue.OrderRequest{Symbol: "BTC", Side: venue.Sell, Size: decimal.RequireFromString("0.0100009")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != venue.OrderFilled || res.OrderID != "77" || !res.FilledSize.Equal(decimal.RequireFromString("0.01")) {
		t.Fatalf("unexpected result %+v", res)
	}
	if len(fake.exchange) != 1 {
		t.Fatalf("want 1 exchange call, got %d", len(fake.exchange))
	}
	var posted struct {
		Action    OrderAction       `json:"action"`
		Nonce     uint64            `json:"nonce"`
		Signature evmsign.Signature `json:"signature"`
	}
	b, _ := json.Marshal(fake.exchange[0])
	if err := json.Unmarshal(b, &posted); err != nil {
		t.Fatal(err)
	}
	o := posted.Action.Orders[0]
	// Sell crosses the bid: 60000 * 0.99 = 59400; size truncates to 5 decimals.
	if o.IsBuy || o.Price != "59400" || o.Size != "0.01" || o.Type.Limit.TIF != "Ioc" || o.Asset != 0 {
		t.Fatalf("unexpected order wire %+v", o)
	}
	if posted.Action.Builder == nil || posted.Action.Builder.B != "0xf945417968b53adc1e0489e5a287d2e41c8b3d79" {
		t.Fatalf("builder must be lowercased: %+v", posted.Action.Builder)
	}
	hash, _ := ActionHash(posted.Action, L1Params{Nonce: posted.Nonce})
	digest, _, _ := apitypes.TypedDataAndHash(L1TypedData(hash, Mainnet))
	if addr, err := evmsign.RecoverAddress(digest, posted.Signature); err != nil || addr != signer.Address() {
		t.Fatalf("signature recovers to %s (%v), want agent %s", addr, err, signer.Address())
	}
}

func TestPositionReadsMasterAccount(t *testing.T) {
	v, _, _ := newTestVenue(t)
	p, err := v.Position(context.Background(), "BTC")
	if err != nil || !p.Size.Equal(decimal.RequireFromString("-0.01")) {
		t.Fatalf("got %+v, %v", p, err)
	}
	flat, err := v.Position(context.Background(), "ETH")
	if err != nil || !flat.Size.IsZero() {
		t.Fatalf("ETH should be flat: %+v, %v", flat, err)
	}
}

func TestCancelSurfacesVenueError(t *testing.T) {
	v, _, _ := newTestVenue(t)
	if err := v.CancelOrder(context.Background(), "BTC", "77"); err == nil {
		t.Fatal("expected cancel error")
	}
}

func TestLeverageBounds(t *testing.T) {
	v, fake, _ := newTestVenue(t)
	if err := v.SetLeverage(context.Background(), "ETH", 26, true); err == nil {
		t.Fatal("expected max leverage error")
	}
	if len(fake.exchange) != 0 {
		t.Fatal("rejected leverage must not reach the venue")
	}
}

func TestDryRunNeverPosts(t *testing.T) {
	v, fake, _ := newTestVenue(t)
	v.exchange.DryRun = true
	res, err := v.PlaceOrder(context.Background(), venue.OrderRequest{Symbol: "ETH", Side: venue.Buy, Size: decimal.NewFromInt(1), LimitPrice: decimal.NewFromInt(2000)})
	if err != nil || res.Status != venue.OrderPending || len(fake.exchange) != 0 {
		t.Fatalf("dry run posted or failed: %+v %v %d", res, err, len(fake.exchange))
	}
}

func TestNonceIsStrictlyIncreasing(t *testing.T) {
	n := NewNonceSource(nil)
	prev := n.Next()
	for range 1000 {
		next := n.Next()
		if next <= prev {
			t.Fatalf("nonce went %d -> %d", prev, next)
		}
		prev = next
	}
}
