package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
)

// registerWeb serves a loopback page where a browser wallet (OKX, MetaMask,
// Rabby) signs RegisterSessionKey for the session key in
// PAPER_SESSION_PRIVATE_KEY. The master key never leaves the wallet; this
// process only sees the address and the signature.
func registerWeb(ctx context.Context, addr string, days int, live bool) error {
	key, err := evmsign.NewLocalSigner(os.Getenv("PAPER_SESSION_PRIVATE_KEY"))
	if err != nil {
		return fmt.Errorf("PAPER_SESSION_PRIVATE_KEY: %w", err)
	}
	var want common.Address
	if raw := os.Getenv("PAPER_ACCOUNT_ADDRESS"); raw != "" {
		if !common.IsHexAddress(raw) {
			return errors.New("PAPER_ACCOUNT_ADDRESS is not an address")
		}
		want = common.HexToAddress(raw)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") && !strings.HasPrefix(addr, "localhost:") {
		return errors.New("-listen must be a loopback address")
	}
	client := paperClient(!live)

	var (
		mu      sync.Mutex
		pending apitypes.TypedData
		user    common.Address
	)
	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, registerPage, key.Address().Hex(), live)
	})
	mux.HandleFunc("POST /typed-data", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Address string }
		if json.NewDecoder(r.Body).Decode(&req) != nil || !common.IsHexAddress(req.Address) {
			http.Error(w, "bad address", http.StatusBadRequest)
			return
		}
		u := common.HexToAddress(req.Address)
		if want != (common.Address{}) && u != want {
			http.Error(w, fmt.Sprintf("wallet %s is not PAPER_ACCOUNT_ADDRESS %s", u.Hex(), want.Hex()), http.StatusBadRequest)
			return
		}
		if u == key.Address() {
			http.Error(w, "the wallet is the session key itself", http.StatusBadRequest)
			return
		}
		nonce, err := paper.NewNonces(nil).Take(paper.NonceExchange)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		td, err := paper.RegisterSessionKeyTypedData(u, key.Address(), now.Add(time.Duration(days)*24*time.Hour), nonce, now.Add(paper.DefaultIntentTTL))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		pending, user = td, u
		mu.Unlock()
		json.NewEncoder(w).Encode(td)
	})
	mux.HandleFunc("POST /signature", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Signature string }
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		td, u := pending, user
		mu.Unlock()
		if td.PrimaryType == "" {
			http.Error(w, "request typed data first", http.StatusBadRequest)
			return
		}
		sig, err := parseSignature(req.Signature)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		in, err := paper.AssembleIntent(paper.ActionRegisterSessionKey, td, sig)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if in.Signer != u {
			http.Error(w, fmt.Sprintf("signature recovers %s, not %s", in.Signer.Hex(), u.Hex()), http.StatusBadRequest)
			return
		}
		res, err := client.Submit(r.Context(), in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out := map[string]any{"account": u.Hex(), "sessionKey": key.Address().Hex(), "intentId": in.ID.Hex(), "result": res}
		json.NewEncoder(w).Encode(out)
		printJSON(out, nil)
		select {
		case done <- nil:
		default:
		}
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	fmt.Fprintf(os.Stderr, "Open http://%s in the browser that has the wallet; session key %s, live=%v\n", ln.Addr(), key.Address().Hex(), live)
	select {
	case err = <-done:
		time.Sleep(500 * time.Millisecond) // let the page read the response
	case <-ctx.Done():
		err = ctx.Err()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return err
}

const registerPage = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><title>Paper session key</title>
<style>body{font:15px system-ui;max-width:640px;margin:40px auto;padding:0 16px}button{font-size:15px;padding:8px 16px}pre{background:#f4f4f4;padding:12px;white-space:pre-wrap;word-break:break-all}</style>
</head><body>
<h2>注册 Papertrade session key</h2>
<p>Session key：<code>%s</code>（只能交易/质押，不能提现）。live=%v</p>
<p>点击后钱包会请求连接、切换到 HyperEVM（chainId 999），并签名 RegisterSessionKey。请核对弹窗里的 key 地址与上面一致。</p>
<button id="go">连接钱包并签名</button>
<pre id="log"></pre>
<script>
const log = m => document.getElementById('log').textContent += m + '\n';
async function post(path, body) {
  const r = await fetch(path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)});
  const t = await r.text();
  if (!r.ok) throw new Error(t);
  return JSON.parse(t);
}
document.getElementById('go').onclick = async () => {
  try {
    const p = window.okxwallet || window.ethereum;
    if (!p) throw new Error('没有检测到钱包扩展');
    const [address] = await p.request({method: 'eth_requestAccounts'});
    log('钱包: ' + address);
    try {
      await p.request({method: 'wallet_switchEthereumChain', params: [{chainId: '0x3e7'}]});
    } catch (e) {
      if (e.code !== 4902) throw e;
      await p.request({method: 'wallet_addEthereumChain', params: [{chainId: '0x3e7', chainName: 'HyperEVM',
        nativeCurrency: {name: 'HYPE', symbol: 'HYPE', decimals: 18}, rpcUrls: ['https://rpc.hyperliquid.xyz/evm'],
        blockExplorerUrls: ['https://hyperevmscan.io']}]});
    }
    const td = await post('/typed-data', {address});
    log('待签名: ' + JSON.stringify(td.message));
    const signature = await p.request({method: 'eth_signTypedData_v4', params: [address, JSON.stringify(td)]});
    const res = await post('/signature', {signature});
    log('完成: ' + JSON.stringify(res, null, 2));
  } catch (e) {
    log('失败: ' + (e.message || e));
  }
};
</script>
</body></html>
`
