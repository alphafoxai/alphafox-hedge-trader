package hedge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/venue"
)

var d = decimal.RequireFromString

// bookVenue is an in-memory hedge venue that fills market orders at the top
// of book and records the call order in a shared log.
type bookVenue struct {
	log      *[]string
	quote    venue.Quote
	position decimal.Decimal
	// fillRatio scales fills to simulate partial IOC fills.
	fillRatio decimal.Decimal
}

func (b *bookVenue) Name() string { return "book" }
func (b *bookVenue) Quote(context.Context, string) (venue.Quote, error) {
	return b.quote, nil
}
func (b *bookVenue) PlaceOrder(_ context.Context, r venue.OrderRequest) (venue.OrderResult, error) {
	*b.log = append(*b.log, "hedge:"+string(r.Side)+":"+r.Size.String()+map[bool]string{true: ":reduce"}[r.ReduceOnly])
	filled := r.Size.Mul(b.fillRatio)
	if r.Side == venue.Sell {
		b.position = b.position.Sub(filled)
	} else {
		b.position = b.position.Add(filled)
	}
	return venue.OrderResult{Status: venue.OrderFilled, FilledSize: filled, AvgPrice: b.quote.Mid()}, nil
}
func (b *bookVenue) CancelOrder(context.Context, string, string) error { return nil }
func (b *bookVenue) Position(_ context.Context, s string) (venue.Position, error) {
	return venue.Position{Symbol: s, Size: b.position}, nil
}
func (b *bookVenue) SetLeverage(context.Context, string, int, bool) error { return nil }

// loggingVenue records mint-leg orders into the same log.
type loggingVenue struct {
	venue.Venue
	log *[]string
}

func (l loggingVenue) PlaceOrder(ctx context.Context, r venue.OrderRequest) (venue.OrderResult, error) {
	*l.log = append(*l.log, "mint:"+string(r.Side)+":"+r.Size.String()+map[bool]string{true: ":reduce"}[r.ReduceOnly])
	return l.Venue.PlaceOrder(ctx, r)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type fixture struct {
	log    []string
	book   *bookVenue
	clock  *clock
	engine *Engine
}

func newFixture(t *testing.T, gatewayReady bool) *fixture {
	t.Helper()
	f := &fixture{clock: &clock{t: time.Unix(1_760_000_000, 0)}}
	f.book = &bookVenue{log: &f.log, fillRatio: d("1"),
		quote: venue.Quote{Bid: d("60000"), BidSize: d("2"), Ask: d("60001"), AskSize: d("2")}}
	var gw paper.Gateway
	if gatewayReady {
		gw = paper.NewSimulatedGateway(func(context.Context, string) (decimal.Decimal, error) { return d("60000.5"), nil },
			30*time.Second, f.clock.now)
	}
	pv, err := paper.NewVenue(paper.Config{Leverage: 100}, gw)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Config{
		Mint: Leg{Venue: loggingVenue{Venue: pv, log: &f.log}, Symbol: "BTC"}, Hedge: Leg{Venue: f.book, Symbol: "BTC"},
		MaxSpreadBps: d("2"), MaxBookShare: d("0.5"), Slippage: d("0.005"),
		CloseTimeout: time.Minute, PollInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Advance the simulated clock instead of sleeping.
	e.sleep = func(ctx context.Context, dur time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.clock.t = f.clock.t.Add(dur)
		return nil
	}
	f.engine = e
	return f
}

func TestOpenPlacesHedgeBeforeMintSizedByFill(t *testing.T) {
	f := newFixture(t, true)
	f.book.fillRatio = d("0.5")
	res, err := f.engine.Open(context.Background(), venue.Buy, d("0.4"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hedge:sell:0.4", "mint:buy:0.2"}
	if strings.Join(f.log, ",") != strings.Join(want, ",") {
		t.Fatalf("order sequence %v, want %v", f.log, want)
	}
	if !res.Mint.FilledSize.Equal(d("0.2")) {
		t.Fatalf("mint size %s", res.Mint.FilledSize)
	}
}

func TestOpenRefusesWideSpreadAndThinBook(t *testing.T) {
	f := newFixture(t, true)
	f.book.quote.Ask = d("60100")
	if _, err := f.engine.Open(context.Background(), venue.Buy, d("0.1")); err == nil || !strings.Contains(err.Error(), "spread") {
		t.Fatalf("want spread error, got %v", err)
	}
	f.book.quote.Ask = d("60001")
	if _, err := f.engine.Open(context.Background(), venue.Buy, d("1.5")); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("want depth error, got %v", err)
	}
	if len(f.log) != 0 {
		t.Fatalf("no order may be sent on a rejected open: %v", f.log)
	}
}

func TestOpenRefusesBeforeAnyOrderWhenPaperIsNotWired(t *testing.T) {
	f := newFixture(t, false)
	_, err := f.engine.Open(context.Background(), venue.Sell, d("0.1"))
	if !errors.Is(err, venue.ErrNotImplemented) {
		t.Fatalf("want ErrNotImplemented, got %v", err)
	}
	if len(f.log) != 0 {
		t.Fatalf("no order may be sent: %v", f.log)
	}
}

// failingMint accepts quotes but rejects every order.
type failingMint struct{ loggingVenue }

func (m failingMint) PlaceOrder(ctx context.Context, r venue.OrderRequest) (venue.OrderResult, error) {
	*m.log = append(*m.log, "mint:"+string(r.Side)+":"+r.Size.String())
	return venue.OrderResult{Status: venue.OrderRejected, Error: "margin"}, nil
}

func TestOpenUnwindsHedgeWhenMintLegFails(t *testing.T) {
	f := newFixture(t, true)
	mint := f.engine.cfg.Mint.Venue.(loggingVenue)
	f.engine.cfg.Mint.Venue = failingMint{mint}
	_, err := f.engine.Open(context.Background(), venue.Sell, d("0.1"))
	if err == nil || errors.Is(err, ErrUnhedged) || !strings.Contains(err.Error(), "hedge unwound") {
		t.Fatalf("want unwound error, got %v", err)
	}
	if !f.book.position.IsZero() {
		t.Fatalf("hedge must be unwound, position %s", f.book.position)
	}
	want := []string{"hedge:buy:0.1", "mint:sell:0.1", "hedge:sell:0.1:reduce"}
	if strings.Join(f.log, ",") != strings.Join(want, ",") {
		t.Fatalf("order sequence %v, want %v", f.log, want)
	}
}

func TestCloseWaitsForQueuedPaperCloseBeforeHedge(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.engine.Open(context.Background(), venue.Buy, d("0.3")); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	start := f.clock.t
	if _, err := f.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"mint:sell:0.3:reduce", "hedge:buy:0.3:reduce"}
	if strings.Join(f.log, ",") != strings.Join(want, ",") {
		t.Fatalf("order sequence %v, want %v", f.log, want)
	}
	if f.clock.t.Sub(start) < 30*time.Second {
		t.Fatalf("hedge closed before the queued paper close settled (%s)", f.clock.t.Sub(start))
	}
	if !f.book.position.IsZero() {
		t.Fatalf("hedge not flat: %s", f.book.position)
	}
}

func TestCloseKeepsHedgeWhenPaperNeverSettles(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.engine.Open(context.Background(), venue.Buy, d("0.3")); err != nil {
		t.Fatal(err)
	}
	// Freeze the simulated clock so the queued close never settles, and let
	// the real-time CloseTimeout expire.
	f.engine.sleep = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
	f.engine.cfg.CloseTimeout = 20 * time.Millisecond
	f.log = nil
	_, err := f.engine.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hedge kept open") {
		t.Fatalf("want hedge-kept error, got %v", err)
	}
	if f.book.position.IsZero() {
		t.Fatal("hedge must stay open while paper is still exposed")
	}
	if len(f.log) != 1 {
		t.Fatalf("only the mint close may be sent: %v", f.log)
	}
}
