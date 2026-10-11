package main

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/hyperliquid"
)

const (
	tradingBody = `{"tradingPaused":false,"minimumTradeSizeRaw":"10000000000000000000","minimumOpenNotionalRaw":"10000000000000000000000","instruments":[[0,"BTC",0,"10",true,false,false,false,false,null,null,null,"15000000000000000000000","814598000000000000000",1000,"10000000000000000000000000","0","100000000000000000","100000000000000000000000","0","0","0","0"]]}`
	summaryBody = `{"ok":true,"balances":{"queue":"0"},"fees":{"lifetime":"0"},"paper":{"supply":"0","staked":"0","trackedLp":"0","tailProgress":"77991670838876812565013732","rate":"100000000000000000000","decay":"120000000000000000000000000"}}`
)

// fakeRelayer serves the read endpoints and fails the test on any write.
func fakeRelayer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method != http.MethodGet:
			t.Errorf("dry run posted %s %s", r.Method, r.URL)
		case r.URL.Path == "/state/trading":
			io.WriteString(w, tradingBody)
		case r.URL.Path == "/query/protocol/summary":
			io.WriteString(w, summaryBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	key, _ := crypto.GenerateKey()
	t.Setenv("PAPER_RELAYER_URL", srv.URL)
	t.Setenv("PAPER_ACCOUNT_ADDRESS", "0xacadbd12c0eb153bb3e56f337eb34850325a6cec")
	t.Setenv("PAPER_SESSION_PRIVATE_KEY", "0x"+hex.EncodeToString(crypto.FromECDSA(key)))
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	stdout := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = stdout; devnull.Close() })
}

func TestDryRunsPostNothing(t *testing.T) {
	fakeRelayer(t)
	for _, args := range [][]string{
		{"farm", "run", "-margin", "10", "-leverage", "1000", "-close-move", "0.0003", "-bust-guard", "0.0001"},
		{"paper", "open", "-coin", "BTC", "-side", "long", "-margin", "10", "-leverage", "1000"},
		{"paper", "close", "-ids", "131656,131659"},
		{"paper", "stake", "-amount", "100"},
		{"paper", "claim"},
		{"paper", "econ", "-notional", "10000"},
	} {
		if err := run(context.Background(), args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestFarmRejectsUnsafePlans(t *testing.T) {
	fakeRelayer(t)
	for want, args := range map[string][]string{
		"below venue minimum": {"farm", "run", "-margin", "10", "-leverage", "500"},
		"bust distance":       {"farm", "run", "-margin", "10", "-leverage", "1000", "-close-move", "0.0005"},
		"above BTC max":       {"farm", "run", "-margin", "10", "-leverage", "2000"},
	} {
		if err := run(context.Background(), args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err %v, want %q", args, err, want)
		}
	}
}

func TestSessionKeyMustNotBeMaster(t *testing.T) {
	fakeRelayer(t)
	key, _ := crypto.GenerateKey()
	t.Setenv("PAPER_ACCOUNT_ADDRESS", crypto.PubkeyToAddress(key.PublicKey).Hex())
	t.Setenv("PAPER_SESSION_PRIVATE_KEY", "0x"+hex.EncodeToString(crypto.FromECDSA(key)))
	if err := run(context.Background(), []string{"paper", "claim"}); err == nil || !strings.Contains(err.Error(), "master wallet key") {
		t.Fatalf("err %v", err)
	}
}

func TestParseSignature(t *testing.T) {
	r, s := strings.Repeat("11", 32), strings.Repeat("22", 32)
	for in, v := range map[string]int{"0x" + r + s + "1b": 27, r + s + "01": 28} {
		sig, err := parseSignature(in)
		if err != nil || sig.V != v || sig.R != "0x"+r || sig.S != "0x"+s {
			t.Errorf("%s: %+v %v", in, sig, err)
		}
	}
	if _, err := parseSignature("0x1234"); err == nil {
		t.Error("short signature accepted")
	}
}

func TestHyperliquidMid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"coin":"BTC","levels":[[{"px":"83000","sz":"1","n":1}],[{"px":"83001","sz":"1","n":1}]]}`)
	}))
	defer srv.Close()
	px, err := hyperliquidMid(hyperliquid.NewInfoClientWithBaseURL(srv.URL, nil))(context.Background(), "BTC")
	if err != nil || !px.Equal(decimal.RequireFromString("83000.5")) {
		t.Fatalf("mid %s, %v", px, err)
	}
}
