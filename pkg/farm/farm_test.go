package farm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var d = decimal.RequireFromString

// fakeVenue simulates Paper: opens fill at the current price after a delay,
// closes settle every listed id at one price, and the history reports a
// simplified settlement (winner keeps 70% of its gain, loser mints 36/USD).
type fakeVenue struct {
	mu        sync.Mutex
	now       time.Time
	prices    []decimal.Decimal // consumed one per price read; last repeats
	fillDelay time.Duration
	// failShort makes the short Open return an error; dropShort accepts it
	// but never fills it.
	failShort, dropShort bool
	// ignoreCloses swallows this many Close calls.
	ignoreCloses int

	nextID   int
	pending  []pendingOpen
	open     []Position
	settled  map[string]Settled
	opens    int
	closes   [][]string
	priceIdx int
}

type pendingOpen struct {
	at     time.Time
	isLong bool
	intent string
	margin decimal.Decimal
	lev    int
}

func newFake(prices ...string) *fakeVenue {
	f := &fakeVenue{now: time.Unix(1_800_000_000, 0), settled: map[string]Settled{}}
	for _, p := range prices {
		f.prices = append(f.prices, d(p))
	}
	return f
}

func (f *fakeVenue) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeVenue) sleep(ctx context.Context, dur time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.now = f.now.Add(dur)
	f.mu.Unlock()
	return nil
}

func (f *fakeVenue) current() decimal.Decimal {
	i := f.priceIdx
	if i >= len(f.prices) {
		i = len(f.prices) - 1
	}
	return f.prices[i]
}

func (f *fakeVenue) price(ctx context.Context, symbol string) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.current()
	f.priceIdx++
	return p, nil
}

func (f *fakeVenue) Open(ctx context.Context, symbol string, isLong bool, margin decimal.Decimal, lev int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if !isLong && f.failShort {
		return "", errors.New("relayer rejected")
	}
	intent := fmt.Sprintf("0xintent%d", f.opens)
	if !isLong && f.dropShort {
		return intent, nil
	}
	f.pending = append(f.pending, pendingOpen{at: f.now.Add(f.fillDelay), isLong: isLong, intent: intent, margin: margin, lev: lev})
	return intent, nil
}

func (f *fakeVenue) Close(ctx context.Context, ids []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, append([]string(nil), ids...))
	if f.ignoreCloses > 0 {
		f.ignoreCloses--
		return "0xclose-ignored", nil
	}
	// Settle at the last price the strategy saw.
	exit := f.prices[0]
	if f.priceIdx > 0 {
		exit = f.prices[min(f.priceIdx, len(f.prices))-1]
	}
	keep := f.open[:0:0]
	for _, p := range f.open {
		if !contains(ids, p.ID) {
			keep = append(keep, p)
			continue
		}
		notional := p.Margin.Mul(decimal.NewFromInt(int64(p.Leverage)))
		move := exit.Sub(p.Entry).Div(p.Entry)
		if !p.IsLong {
			move = move.Neg()
		}
		pnl := notional.Mul(move)
		s := Settled{PositionID: p.ID, IsLong: p.IsLong, Kind: "PositionClosed", Exit: exit}
		if pnl.IsNegative() {
			s.PnL = pnl
			s.PaperMinted = pnl.Neg().Mul(d("36"))
		} else {
			s.PnL = pnl.Mul(d("0.7"))
		}
		f.settled[p.ID] = s
	}
	f.open = keep
	return fmt.Sprintf("0xclose%d", len(f.closes)), nil
}

func (f *fakeVenue) Positions(ctx context.Context) ([]Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	still := f.pending[:0:0]
	for _, p := range f.pending {
		if f.now.Before(p.at) {
			still = append(still, p)
			continue
		}
		f.nextID++
		entry := f.current()
		room := decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(p.lev))).Sub(d("0.0005"))
		bust := entry.Mul(decimal.NewFromInt(1).Sub(room))
		if !p.isLong {
			bust = entry.Mul(decimal.NewFromInt(1).Add(room))
		}
		f.open = append(f.open, Position{ID: fmt.Sprint(100 + f.nextID), IntentID: p.intent, Symbol: "BTC",
			IsLong: p.isLong, Entry: entry, Bust: bust, Margin: p.margin, Leverage: p.lev})
	}
	f.pending = still
	return append([]Position(nil), f.open...), nil
}

func (f *fakeVenue) Settled(ctx context.Context, ids []string) ([]Settled, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Settled
	for _, id := range ids {
		if s, ok := f.settled[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{
		Symbol: "BTC", Margin: d("100"), Leverage: 100,
		CloseMove: d("0.005"), BustGuard: d("0.002"),
		FillTimeout: time.Minute, CloseTimeout: 30 * time.Second, CloseRetries: 1,
		PollInterval: time.Second, MinMargin: d("10"), MinNotional: d("10000"),
	}
}

func newFarmer(t *testing.T, cfg Config, f *fakeVenue) *Farmer {
	t.Helper()
	fm, err := New(cfg, f, f.price, WithClock(f.clock, f.sleep))
	if err != nil {
		t.Fatal(err)
	}
	return fm
}

func TestCycleClosesBothLegsInOneIntent(t *testing.T) {
	f := newFake("100000", "100000", "100200", "100400", "100500", "100600")
	f.fillDelay = 2 * time.Second
	res, err := newFarmer(t, testConfig(), f).RunCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonThreshold || !res.ClosePrice.Equal(d("100500")) {
		t.Fatalf("reason %s at %s", res.Reason, res.ClosePrice)
	}
	if len(f.closes) != 1 || len(f.closes[0]) != 2 || f.closes[0][0] != res.Long.ID || f.closes[0][1] != res.Short.ID {
		t.Fatalf("closes %v, want one close of both legs", f.closes)
	}
	// 10,000 notional, 0.5% move: short loses 50 and mints 1800; long keeps 35.
	if !res.NetCost.Equal(d("15")) || !res.PaperMinted.Equal(d("1800")) || !res.CostPerPaper.Equal(d("15").Div(d("1800"))) {
		t.Fatalf("net %s paper %s cpp %s", res.NetCost, res.PaperMinted, res.CostPerPaper)
	}
	if !res.Long.IsLong || res.Short.IsLong || res.Long.IntentID == res.Short.IntentID {
		t.Fatalf("legs %+v / %+v", res.Long, res.Short)
	}
}

func TestShortOpenRejectedClosesLong(t *testing.T) {
	f := newFake("100000")
	f.failShort = true
	_, err := newFarmer(t, testConfig(), f).RunCycle(context.Background())
	if !errors.Is(err, ErrOneLegged) || !strings.Contains(err.Error(), "relayer rejected") {
		t.Fatalf("err %v, want ErrOneLegged with cause", err)
	}
	if len(f.open) != 0 || len(f.closes) != 1 || len(f.closes[0]) != 1 {
		t.Fatalf("open %v closes %v, want the long rescued", f.open, f.closes)
	}
}

func TestShortNeverFillsClosesLongAfterTimeout(t *testing.T) {
	f := newFake("100000")
	f.dropShort = true
	start := f.clock()
	_, err := newFarmer(t, testConfig(), f).RunCycle(context.Background())
	if !errors.Is(err, ErrOneLegged) {
		t.Fatalf("err %v, want ErrOneLegged", err)
	}
	if f.clock().Sub(start) < time.Minute {
		t.Fatal("rescued before the fill timeout")
	}
	if len(f.open) != 0 {
		t.Fatalf("long left open: %v", f.open)
	}
}

func TestBustGuardClosesBeforeThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.CloseMove = d("0.008")
	cfg.BustGuard = d("0.001")
	// Short busts near 100950; 100860 leaves < 0.1% room.
	f := newFake("100000", "100000", "100500", "100860")
	res, err := newFarmer(t, cfg, f).RunCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonBustGuard || !res.ClosePrice.Equal(d("100860")) {
		t.Fatalf("reason %s at %s, want bust guard", res.Reason, res.ClosePrice)
	}
}

func TestMaxHoldCloses(t *testing.T) {
	cfg := testConfig()
	cfg.MaxHold = 10 * time.Second
	f := newFake("100000")
	res, err := newFarmer(t, cfg, f).RunCycle(context.Background())
	if err != nil || res.Reason != ReasonMaxHold {
		t.Fatalf("reason %s err %v, want max hold", res.Reason, err)
	}
}

// legGoneVenue liquidates the short behind the strategy's back.
type legGoneVenue struct{ *fakeVenue }

func (v legGoneVenue) Positions(ctx context.Context) ([]Position, error) {
	ps, err := v.fakeVenue.Positions(ctx)
	if v.fakeVenue.priceIdx >= 3 {
		v.mu.Lock()
		var keep []Position
		for _, p := range v.open {
			if p.IsLong {
				keep = append(keep, p)
			}
		}
		v.open = keep
		v.mu.Unlock()
		return keep, err
	}
	return ps, err
}

func TestLegGoneClosesSurvivor(t *testing.T) {
	f := newFake("100000")
	v := legGoneVenue{f}
	fm, err := New(testConfig(), v, f.price, WithClock(f.clock, f.sleep))
	if err != nil {
		t.Fatal(err)
	}
	res, err := fm.RunCycle(context.Background())
	if res.Reason != ReasonLegGone {
		t.Fatalf("reason %q err %v", res.Reason, err)
	}
	if len(f.open) != 0 {
		t.Fatalf("survivor left open: %v", f.open)
	}
}

func TestRefusesWhenNotFlat(t *testing.T) {
	f := newFake("100000")
	f.open = []Position{{ID: "9", Symbol: "BTC"}}
	_, err := newFarmer(t, testConfig(), f).RunCycle(context.Background())
	if !errors.Is(err, ErrNotFlat) || f.opens != 0 {
		t.Fatalf("err %v opens %d, want refusal before any open", err, f.opens)
	}
}

func TestCloseResubmitsStillOpenLegs(t *testing.T) {
	f := newFake("100000", "100000", "100600")
	f.ignoreCloses = 1
	res, err := newFarmer(t, testConfig(), f).RunCycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.closes) != 2 || len(f.closes[1]) != 2 || res.CloseIntent != "0xclose-ignored" {
		t.Fatalf("closes %v intent %s, want a second close of both legs", f.closes, res.CloseIntent)
	}
}

func TestCloseGivesUpAfterRetries(t *testing.T) {
	f := newFake("100000", "100000", "100600")
	f.ignoreCloses = 5
	_, err := newFarmer(t, testConfig(), f).RunCycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still open after 2 close attempts") {
		t.Fatalf("err %v", err)
	}
}

func TestCancelLeavesPairOpen(t *testing.T) {
	f := newFake("100000")
	ctx, cancel := context.WithCancel(context.Background())
	fm, err := New(testConfig(), f, func(c context.Context, s string) (decimal.Decimal, error) {
		if f.priceIdx >= 2 {
			cancel()
		}
		return f.price(c, s)
	}, WithClock(f.clock, f.sleep))
	if err != nil {
		t.Fatal(err)
	}
	_, err = fm.RunCycle(ctx)
	if !errors.Is(err, context.Canceled) || len(f.open) != 2 || len(f.closes) != 0 {
		t.Fatalf("err %v open %d closes %v, want the hedged pair left alone", err, len(f.open), f.closes)
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"trigger past bust":  func(c *Config) { c.CloseMove = d("0.009") },
		"notional too small": func(c *Config) { c.Margin = d("50") },
		"margin too small":   func(c *Config) { c.Margin, c.Leverage = d("5"), 1000 },
		"1000x with 0.05%":   func(c *Config) { c.Margin, c.Leverage, c.CloseMove, c.BustGuard = d("10"), 1000, d("0.0005"), d("0") },
		"no poll":            func(c *Config) { c.PollInterval = 0 },
	} {
		c := testConfig()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := testConfig()
	ok.Margin, ok.Leverage, ok.CloseMove, ok.BustGuard = d("10"), 1000, d("0.0003"), d("0.0001")
	if err := ok.Validate(); err != nil {
		t.Errorf("20U at 1000x rejected: %v", err)
	}
}
