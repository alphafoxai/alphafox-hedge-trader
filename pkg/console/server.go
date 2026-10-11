// Package console is the local web console for Papertrade farming. It serves
// a single page on a loopback address and proxies the relayer, which refuses
// cross-origin browser requests.
//
// Credentials live in the browser: the page keeps the session key in
// localStorage and sends it with each write. The server never persists it;
// a running farm holds it in memory until the run ends. The master wallet
// only signs RegisterSessionKey in the browser wallet.
//
// Every /api request must carry the per-process token embedded in the page
// and a loopback Host header, so other sites cannot drive the console
// through the browser (CSRF, DNS rebinding).
package console

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

//go:embed static
var static embed.FS

// Symbols are the markets the console farms.
var Symbols = []string{"BTC", "ETH"}

// Exchange is what a run trades through; *paper.Account in production.
type Exchange interface {
	farm.Exchange
	Stake(ctx context.Context, amount *big.Int) (string, error)
}

type Server struct {
	// Live posts intents; otherwise every write is signed and dry-run.
	Live    bool
	Relayer *paper.Client
	Info    *hyperliquid.InfoClient
	// NewExchange builds the venue for an account and session key. Nil uses
	// a paper.Account on Relayer.
	NewExchange func(user common.Address, key evmsign.Signer) Exchange
	// Price is the farm's price source. Nil uses the Hyperliquid mid.
	Price farm.PriceFunc
	// FarmOptions are passed to every farm.New (tests use a fake clock).
	FarmOptions []farm.Option

	token string
	page  *template.Template

	mu     sync.Mutex
	runs   map[string]*Run
	nonces map[common.Address]*paper.Nonces
	vols   map[string]volCache
}

type volCache struct {
	at     time.Time
	window time.Duration
	sigma  float64
	day    float64
}

func New(relayer *paper.Client, info *hyperliquid.InfoClient, live bool) (*Server, error) {
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	page, err := template.ParseFS(static, "static/index.html")
	if err != nil {
		return nil, err
	}
	relayer.DryRun = !live
	return &Server{Live: live, Relayer: relayer, Info: info, token: hex.EncodeToString(tok), page: page,
		runs: map[string]*Run{}, nonces: map[common.Address]*paper.Nonces{}, vols: map[string]volCache{}}, nil
}

// Token is the per-process API token (tests).
func (s *Server) Token() string { return s.token }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(static, "static")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		s.page.Execute(w, map[string]any{"Token": s.token, "Live": s.Live})
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.handleConfig)
	api.HandleFunc("GET /api/market", s.handleMarket)
	api.HandleFunc("GET /api/account", s.handleAccount)
	api.HandleFunc("POST /api/session/new", s.handleSessionNew)
	api.HandleFunc("POST /api/session/address", s.handleSessionAddress)
	api.HandleFunc("POST /api/session/typed-data", s.handleSessionTypedData)
	api.HandleFunc("POST /api/session/register", s.handleSessionRegister)
	api.HandleFunc("GET /api/runs", s.handleRuns)
	api.HandleFunc("POST /api/runs", s.handleStartRun)
	api.HandleFunc("POST /api/runs/{symbol}/stop", s.handleStopRun)
	api.HandleFunc("POST /api/close", s.handleClose)
	api.HandleFunc("POST /api/stake", s.handleStake)
	api.HandleFunc("POST /api/claim", s.handleClaim)
	mux.Handle("/api/", s.guard(api))
	return s.checkHost(mux)
}

func (s *Server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "console only answers on loopback", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Console-Token") != s.token {
			http.Error(w, "missing or wrong console token", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status := http.StatusBadRequest
		var re *paper.RelayerError
		if errors.As(err, &re) {
			status = http.StatusBadGateway
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	return nil
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"live": s.Live, "symbols": Symbols, "chainId": paper.ChainID,
		"exchange": paper.ExchangeAddress.Hex()}, nil)
}

// --- market and recommendations -------------------------------------------

type marketView struct {
	Symbol      string     `json:"symbol"`
	Mid         float64    `json:"mid"`
	Sigma       float64    `json:"sigmaPerSqrtHour"` // rolling window
	SigmaDay    float64    `json:"sigmaDayWindow"`   // last 24h, for context
	MaxLeverage int        `json:"maxLeverage"`
	Plan        paper.Plan `json:"plan"`
	Error       string     `json:"error,omitempty"`
}

func (s *Server) handleMarket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	capital, _ := strconv.ParseFloat(q.Get("capital"), 64)
	hours, _ := strconv.ParseFloat(q.Get("hours"), 64)
	window, err := time.ParseDuration(q.Get("window"))
	if err != nil || window < 30*time.Minute || window > 72*time.Hour {
		window = 6 * time.Hour
	}
	ctx := r.Context()
	ts, err := s.Relayer.Trading(ctx)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	sum, err := s.Relayer.Summary(ctx)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	out := map[string]any{"paused": ts.Paused, "minMargin": ts.MinimumMargin, "minNotional": ts.MinimumNotional,
		"mintRate": sum.MintRate(), "queueEmpty": sum.QueueBalance.IsZero(), "window": window.String(), "at": time.Now()}
	split := len(Symbols)
	var views []marketView
	for _, sym := range Symbols {
		v := marketView{Symbol: sym}
		m, err := ts.Market(sym)
		if err != nil {
			v.Error = err.Error()
			views = append(views, v)
			continue
		}
		v.MaxLeverage = m.MaxLeverage
		if mid, err := s.Info.Mid(ctx, sym); err == nil {
			v.Mid = mid.InexactFloat64()
		}
		vc, err := s.volatility(ctx, sym, window)
		if err != nil {
			v.Error = err.Error()
		}
		v.Sigma, v.SigmaDay = vc.sigma, vc.day
		if capital > 0 {
			v.Plan = paper.Recommend(paper.PlanInput{Symbol: sym, Impact: m.Impact, MaxLeverage: m.MaxLeverage,
				MinMargin: ts.MinimumMargin.InexactFloat64(), MinNotional: ts.MinimumNotional.InexactFloat64(),
				MintRate: sum.MintRate(), QueueEmpty: sum.QueueBalance.IsZero(),
				Capital: capital / float64(split), Sigma: vc.sigma, TargetHours: hours})
		}
		views = append(views, v)
	}
	out["markets"] = views
	writeJSON(w, out, nil)
}

// volatility is the rolling realized σ per √hour from 1m candles over
// window, plus the last 24h for comparison; cached for 30 seconds.
func (s *Server) volatility(ctx context.Context, sym string, window time.Duration) (volCache, error) {
	s.mu.Lock()
	vc, ok := s.vols[sym]
	s.mu.Unlock()
	if ok && vc.window == window && time.Since(vc.at) < 30*time.Second {
		return vc, nil
	}
	end := time.Now()
	span := max(window, 24*time.Hour)
	cs, err := s.Info.Candles(ctx, sym, "1m", end.Add(-span).UnixMilli(), end.UnixMilli())
	if err != nil {
		return vc, err
	}
	var all, recent []float64
	cut := end.Add(-window).UnixMilli()
	dayCut := end.Add(-24 * time.Hour).UnixMilli()
	for _, c := range cs {
		px := c.Close.InexactFloat64()
		if c.OpenTime >= dayCut {
			all = append(all, px)
		}
		if c.OpenTime >= cut {
			recent = append(recent, px)
		}
	}
	vc = volCache{at: time.Now(), window: window, sigma: paper.RealizedVol(recent, 60), day: paper.RealizedVol(all, 60)}
	if vc.sigma == 0 {
		return vc, fmt.Errorf("no %s candles in the last %s", sym, window)
	}
	s.mu.Lock()
	s.vols[sym] = vc
	s.mu.Unlock()
	return vc, nil
}

// --- account ----------------------------------------------------------------

func parseAddress(raw, name string) (common.Address, error) {
	if !common.IsHexAddress(raw) {
		return common.Address{}, fmt.Errorf("%s is not an address", name)
	}
	return common.HexToAddress(raw), nil
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	user, err := parseAddress(r.URL.Query().Get("address"), "address")
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	wallet, err := s.Relayer.Wallet(r.Context(), user)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	trades, err := s.Relayer.Trades(r.Context(), user, 30)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]any{"wallet": wallet, "trades": trades}, nil)
}

// --- session key registration -----------------------------------------------

func (s *Server) handleSessionNew(w http.ResponseWriter, r *http.Request) {
	key, err := crypto.GenerateKey()
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	writeJSON(w, map[string]string{"address": crypto.PubkeyToAddress(key.PublicKey).Hex(),
		"privateKey": "0x" + hex.EncodeToString(crypto.FromECDSA(key))}, nil)
}

// handleSessionAddress derives the address of an imported session key.
func (s *Server) handleSessionAddress(w http.ResponseWriter, r *http.Request) {
	var req struct{ SessionKey string }
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	key, err := evmsign.NewLocalSigner(req.SessionKey)
	if err != nil {
		writeJSON(w, nil, errors.New("session key is not a valid private key"))
		return
	}
	writeJSON(w, map[string]string{"address": key.Address().Hex()}, nil)
}

func (s *Server) handleSessionTypedData(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account, SessionKey string
		Days                int
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	user, err := parseAddress(req.Account, "account")
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	key, err := parseAddress(req.SessionKey, "sessionKey")
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if key == user {
		writeJSON(w, nil, errors.New("the session key must not be the wallet itself"))
		return
	}
	if req.Days < 1 || req.Days > 30 {
		req.Days = 30
	}
	nonce, err := s.noncesFor(user).Take(paper.NonceExchange)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	now := time.Now()
	td, err := paper.RegisterSessionKeyTypedData(user, key, now.Add(time.Duration(req.Days)*24*time.Hour), nonce, now.Add(paper.DefaultIntentTTL))
	writeJSON(w, td, err)
}

func (s *Server) handleSessionRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account   string
		TypedData apitypes.TypedData
		Signature string
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	user, err := parseAddress(req.Account, "account")
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	td := req.TypedData
	if td.PrimaryType != "RegisterSessionKey" || td.Domain.Name != paper.ExchangeDomain().Name ||
		!strings.EqualFold(td.Domain.VerifyingContract, paper.ExchangeAddress.Hex()) {
		writeJSON(w, nil, errors.New("not a Papertrade RegisterSessionKey payload"))
		return
	}
	sig, err := ParseSignature(req.Signature)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	in, err := paper.AssembleIntent(paper.ActionRegisterSessionKey, td, sig)
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if in.Signer != user {
		writeJSON(w, nil, fmt.Errorf("signature recovers %s, not %s", in.Signer.Hex(), user.Hex()))
		return
	}
	res, err := s.Relayer.Submit(r.Context(), in)
	writeJSON(w, map[string]any{"intentId": in.ID.Hex(), "result": res}, err)
}

// ParseSignature reads a 65-byte r‖s‖v hex signature as wallets return it.
func ParseSignature(s string) (evmsign.Signature, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(raw) != 65 {
		return evmsign.Signature{}, errors.New("signature must be 65 bytes of hex")
	}
	v := int(raw[64])
	if v < 27 {
		v += 27
	}
	return evmsign.Signature{R: "0x" + hex.EncodeToString(raw[:32]), S: "0x" + hex.EncodeToString(raw[32:64]), V: v}, nil
}

// --- credentials --------------------------------------------------------------

// creds is what the browser sends with every write.
type creds struct {
	Account    string `json:"account"`
	SessionKey string `json:"sessionKey"` // private key hex
}

func (c creds) parse() (common.Address, *evmsign.LocalSigner, error) {
	user, err := parseAddress(c.Account, "account")
	if err != nil {
		return common.Address{}, nil, err
	}
	key, err := evmsign.NewLocalSigner(c.SessionKey)
	if err != nil {
		return common.Address{}, nil, errors.New("session key is not a valid private key")
	}
	if key.Address() == user {
		return common.Address{}, nil, errors.New("that is the wallet's own key; use a session key")
	}
	return user, key, nil
}

func (s *Server) noncesFor(user common.Address) *paper.Nonces {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nonces[user]
	if !ok {
		n = paper.NewNonces(nil)
		s.nonces[user] = n
	}
	return n
}

func (s *Server) account(user common.Address, key evmsign.Signer, ttl time.Duration) *paper.Account {
	return &paper.Account{Client: s.Relayer, Intents: paper.NewIntents(user, key, s.noncesFor(user), ttl, nil)}
}

func (s *Server) exchange(user common.Address, key evmsign.Signer) Exchange {
	if s.NewExchange != nil {
		return s.NewExchange(user, key)
	}
	return s.account(user, key, 0)
}

// --- manual actions -------------------------------------------------------------

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		creds
		IDs []string `json:"ids"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	user, key, err := req.parse()
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	if s.anyRunning() {
		writeJSON(w, nil, errors.New("a farm run is active; use its stop-and-close instead"))
		return
	}
	id, err := s.exchange(user, key).Close(r.Context(), req.IDs)
	writeJSON(w, map[string]any{"intentId": id, "dryRun": !s.Live}, err)
}

func (s *Server) handleStake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		creds
		Amount string `json:"amount"` // PAPER; empty stakes the whole unstaked balance
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	user, key, err := req.parse()
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	var amt *big.Int
	if req.Amount == "" {
		wl, err := s.Relayer.Wallet(r.Context(), user)
		if err != nil {
			writeJSON(w, nil, err)
			return
		}
		amt, _ = new(big.Int).SetString(wl.Paper.Balance, 10)
	} else {
		d, err := decimal.NewFromString(req.Amount)
		if err != nil {
			writeJSON(w, nil, fmt.Errorf("amount: %w", err))
			return
		}
		amt = paper.ToRaw(d)
	}
	if amt == nil || amt.Sign() <= 0 {
		writeJSON(w, nil, errors.New("nothing to stake"))
		return
	}
	id, err := s.exchange(user, key).Stake(r.Context(), amt)
	writeJSON(w, map[string]any{"intentId": id, "dryRun": !s.Live}, err)
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req creds
	if err := decode(r, &req); err != nil {
		writeJSON(w, nil, err)
		return
	}
	user, key, err := req.parse()
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	in, err := s.account(user, key, 0).Intents.Claim()
	if err != nil {
		writeJSON(w, nil, err)
		return
	}
	res, err := s.Relayer.Submit(r.Context(), in)
	writeJSON(w, map[string]any{"intentId": in.ID.Hex(), "result": res}, err)
}
