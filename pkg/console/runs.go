package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/farm"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
)

const (
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateDone     = "done"
	StateFailed   = "failed"

	maxLogLines = 300
	maxCycles   = 1000
)

// RunRequest starts farming one symbol.
type RunRequest struct {
	creds
	Symbol         string  `json:"symbol"`
	Margin         string  `json:"margin"` // per leg, USD
	Leverage       int     `json:"leverage"`
	CloseMove      string  `json:"closeMove"`
	BustGuard      string  `json:"bustGuard"`
	MaxHoldMinutes float64 `json:"maxHoldMinutes"`
	Cycles         int     `json:"cycles"`
	StakeEachCycle bool    `json:"stakeEachCycle"`
}

// Run is one farming session on a symbol.
type Run struct {
	Symbol    string        `json:"symbol"`
	State     string        `json:"state"`
	Config    RunRequest    `json:"config"`
	Cycle     int           `json:"cycle"`
	StartedAt time.Time     `json:"startedAt"`
	EndedAt   time.Time     `json:"endedAt,omitzero"`
	Results   []farm.Result `json:"results"`
	OpenPair  []string      `json:"openPair,omitempty"` // left open by a stop
	Error     string        `json:"error,omitempty"`
	Logs      []string      `json:"logs"`
	NetCost   string        `json:"netCost"`
	Paper     string        `json:"paperMinted"`

	mu         sync.Mutex
	cancel     context.CancelFunc
	closeOnEnd bool
	done       chan struct{}
}

func (r *Run) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Logs = append(r.Logs, time.Now().Format("15:04:05 ")+fmt.Sprintf(format, args...))
	if len(r.Logs) > maxLogLines {
		r.Logs = r.Logs[len(r.Logs)-maxLogLines:]
	}
}

func (r *Run) view() Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	cost, minted := decimal.Zero, decimal.Zero
	for _, res := range r.Results {
		cost, minted = cost.Add(res.NetCost), minted.Add(res.PaperMinted)
	}
	cfg := r.Config
	cfg.SessionKey = "" // never echo the key
	return Run{Symbol: r.Symbol, State: r.State, Config: cfg, Cycle: r.Cycle, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		Results: append([]farm.Result(nil), r.Results...), OpenPair: r.OpenPair, Error: r.Error,
		Logs: append([]string(nil), r.Logs...), NetCost: cost.StringFixed(4), Paper: minted.StringFixed(2)}
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	runs := make([]*Run, 0, len(s.runs))
	for _, run := range s.runs {
		runs = append(runs, run)
	}
	s.mu.Unlock()
	sort.Slice(runs, func(i, j int) bool { return runs[i].Symbol < runs[j].Symbol })
	out := make([]Run, len(runs))
	for i, run := range runs {
		out[i] = run.view()
	}
	writeJSON(w, out, nil)
}

func (s *Server) anyRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.runs {
		run.mu.Lock()
		st := run.State
		run.mu.Unlock()
		if st == StateRunning || st == StateStopping {
			return true
		}
	}
	return false
}

func (s *Server) farmConfig(ctx context.Context, req RunRequest) (farm.Config, paper.TradingState, error) {
	ts, err := s.Relayer.Trading(ctx)
	if err != nil {
		return farm.Config{}, ts, err
	}
	m, err := ts.Market(req.Symbol)
	if err != nil {
		return farm.Config{}, ts, err
	}
	if req.Leverage > m.MaxLeverage {
		return farm.Config{}, ts, fmt.Errorf("leverage %d above %s max %d", req.Leverage, req.Symbol, m.MaxLeverage)
	}
	cfg := farm.Config{
		Symbol: req.Symbol, Leverage: req.Leverage,
		MaxHold:     time.Duration(req.MaxHoldMinutes * float64(time.Minute)),
		FillTimeout: 30 * time.Second, CloseTimeout: 30 * time.Second, CloseRetries: 3, PollInterval: time.Second,
		MinMargin: ts.MinimumMargin, MinNotional: ts.MinimumNotional,
	}
	for _, f := range []struct {
		name, src string
		dst       *decimal.Decimal
	}{{"margin", req.Margin, &cfg.Margin}, {"closeMove", req.CloseMove, &cfg.CloseMove}, {"bustGuard", req.BustGuard, &cfg.BustGuard}} {
		if *f.dst, err = decimal.NewFromString(f.src); err != nil {
			return cfg, ts, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return cfg, ts, cfg.Validate()
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	user, key, err := req.parse()
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if req.Cycles < 1 || req.Cycles > maxCycles {
		writeJSON(w, nil, fmt.Errorf("cycles must be 1..%d", maxCycles))
		return
	}
	cfg, ts, err := s.farmConfig(r.Context(), req)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if !s.Live {
		// Plan only: sign the two opens to show exactly what would be posted.
		in, _ := paper.InstrumentBySymbol(req.Symbol)
		intents := s.account(user, key, 0).Intents
		long, err := intents.Open(in, true, paper.ToRaw(cfg.Margin), cfg.Leverage)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		short, err := intents.Open(in, false, paper.ToRaw(cfg.Margin), cfg.Leverage)
		writeJSON(w, map[string]any{"dryRun": true, "openIntents": []paper.Intent{long, short}}, err)
		return
	}

	if ts.Paused {
		writeJSON(w, nil, errors.New("paper trading is paused"))
		return
	}
	wallet, err := s.Relayer.Wallet(r.Context(), user)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if !wallet.HasSessionKey(key.Address(), time.Now().Add(time.Hour)) {
		writeJSON(w, nil, errors.New("this session key is not registered (or expires within an hour) for the account"))
		return
	}

	s.mu.Lock()
	if old, ok := s.runs[req.Symbol]; ok {
		old.mu.Lock()
		busy := old.State == StateRunning || old.State == StateStopping
		old.mu.Unlock()
		if busy {
			s.mu.Unlock()
			writeJSON(w, nil, fmt.Errorf("%s is already running", req.Symbol))
			return
		}
	}
	// Two symbols farm side by side from one balance; reserve both legs of
	// every running symbol before admitting another.
	reserved := cfg.Margin.Mul(decimal.NewFromInt(2))
	for sym, run := range s.runs {
		run.mu.Lock()
		if sym != req.Symbol && run.State == StateRunning {
			if m, err := decimal.NewFromString(run.Config.Margin); err == nil {
				reserved = reserved.Add(m.Mul(decimal.NewFromInt(2)))
			}
		}
		run.mu.Unlock()
	}
	available, err := paper.FromRaw(wallet.Funds.AvailableBalance)
	if err == nil && available.LessThan(reserved) {
		err = fmt.Errorf("available balance %s is below the %s all running pairs need", available.StringFixed(2), reserved)
	}
	if err != nil {
		s.mu.Unlock()
		writeJSON(w, nil, err)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &Run{Symbol: req.Symbol, State: StateRunning, Config: req, StartedAt: time.Now(), cancel: cancel, done: make(chan struct{})}
	s.runs[req.Symbol] = run
	s.mu.Unlock()

	ex := s.exchange(user, key)
	price := s.Price
	if price == nil {
		price = func(ctx context.Context, sym string) (decimal.Decimal, error) { return s.Info.Mid(ctx, sym) }
	}
	f, err := farm.New(cfg, ex, price, append([]farm.Option{farm.WithLogger(run.logf)}, s.FarmOptions...)...)
	if err != nil {
		cancel()
		run.finish(StateFailed, err)
		writeJSON(w, nil, err)
		return
	}
	go s.loop(ctx, run, f, ex)
	writeJSON(w, run.view(), nil)
}

func (s *Server) loop(ctx context.Context, run *Run, f *farm.Farmer, ex Exchange) {
	defer close(run.done)
	for i := 1; i <= run.Config.Cycles; i++ {
		run.mu.Lock()
		run.Cycle = i
		run.mu.Unlock()
		run.logf("cycle %d/%d", i, run.Config.Cycles)
		res, err := f.RunCycle(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.stopped(run, ex, res)
				return
			}
			run.finish(StateFailed, err)
			return
		}
		run.mu.Lock()
		run.Results = append(run.Results, res)
		run.mu.Unlock()
		run.logf("cycle %d: %s, net cost %s, PAPER %s", i, res.Reason, res.NetCost.StringFixed(4), res.PaperMinted.StringFixed(2))
		if run.Config.StakeEachCycle && res.PaperMinted.IsPositive() {
			raw := paper.ToRaw(res.PaperMinted)
			if id, err := ex.Stake(context.WithoutCancel(ctx), raw); err != nil {
				run.logf("stake failed: %v", err)
			} else {
				run.logf("staked %s PAPER (%s)", res.PaperMinted.StringFixed(2), id)
			}
		}
		if ctx.Err() != nil {
			run.finish(StateStopped, nil)
			return
		}
	}
	run.finish(StateDone, nil)
}

// stopped records a cancelled cycle. The pair stays open (the legs hedge each
// other) unless the stop asked to close it.
func (s *Server) stopped(run *Run, ex Exchange, res farm.Result) {
	var pair []string
	if res.Long.ID != "" && res.Short.ID != "" {
		pair = []string{res.Long.ID, res.Short.ID}
	}
	run.mu.Lock()
	closeIt := run.closeOnEnd
	run.mu.Unlock()
	if len(pair) == 2 && closeIt {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		id, err := ex.Close(ctx, pair)
		if err != nil {
			run.logf("close on stop failed, pair %v left open: %v", pair, err)
			run.mu.Lock()
			run.OpenPair = pair
			run.mu.Unlock()
		} else {
			run.logf("closed pair %v on stop (%s); check settlement in history", pair, id)
		}
	} else if len(pair) == 2 {
		run.logf("stopped with pair %v open", pair)
		run.mu.Lock()
		run.OpenPair = pair
		run.mu.Unlock()
	}
	run.finish(StateStopped, nil)
}

func (r *Run) finish(state string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.State, r.EndedAt = state, time.Now()
	if err != nil {
		r.Error = err.Error()
	}
}

func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	sym := r.PathValue("symbol")
	s.mu.Lock()
	run, ok := s.runs[sym]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, nil, fmt.Errorf("no run for %s", sym))
		return
	}
	run.mu.Lock()
	if run.State != StateRunning {
		run.mu.Unlock()
		writeJSON(w, run.view(), nil)
		return
	}
	run.State = StateStopping
	run.closeOnEnd = r.URL.Query().Get("close") == "1"
	run.mu.Unlock()
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(90 * time.Second):
	case <-r.Context().Done():
	}
	writeJSON(w, run.view(), nil)
}

// Stop cancels every run and waits for them, closing open pairs when close
// is set (shutdown).
func (s *Server) Stop(close bool) {
	s.mu.Lock()
	runs := make([]*Run, 0, len(s.runs))
	for _, run := range s.runs {
		runs = append(runs, run)
	}
	s.mu.Unlock()
	for _, run := range runs {
		run.mu.Lock()
		active := run.State == StateRunning
		if active {
			run.State, run.closeOnEnd = StateStopping, close
		}
		run.mu.Unlock()
		if active {
			run.cancel()
			<-run.done
		}
	}
}

// OpenPairs lists pairs that stopped runs left open, by symbol.
func (s *Server) OpenPairs() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for sym, run := range s.runs {
		run.mu.Lock()
		if len(run.OpenPair) > 0 {
			out[sym] = append([]string(nil), run.OpenPair...)
		}
		run.mu.Unlock()
	}
	return out
}
