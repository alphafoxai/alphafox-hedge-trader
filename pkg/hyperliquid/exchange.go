package hyperliquid

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
)

// ExchangeClient signs and posts L1 actions to /exchange.
type ExchangeClient struct {
	t       transport
	network Network
	signer  evmsign.Signer
	vault   *common.Address
	nonces  *NonceSource
	// DryRun signs actions but never posts them; Submit returns the payload
	// as an error-free ExchangeResponse with Status "dry_run".
	DryRun bool
}

func NewExchangeClient(network Network, signer evmsign.Signer, client *http.Client) *ExchangeClient {
	return &ExchangeClient{t: newTransport(network.APIBaseURL(), client), network: network, signer: signer, nonces: NewNonceSource(nil)}
}

// NewExchangeClientWithBaseURL targets a custom endpoint (tests, proxies);
// the network still decides the signed agent source.
func NewExchangeClientWithBaseURL(baseURL string, network Network, signer evmsign.Signer, client *http.Client) *ExchangeClient {
	c := NewExchangeClient(network, signer, client)
	c.t = newTransport(strings.TrimRight(baseURL, "/"), client)
	return c
}

// WithVault trades a vault or subaccount instead of the signer's master.
func (c *ExchangeClient) WithVault(vault common.Address) *ExchangeClient {
	c.vault = &vault
	return c
}

type ExchangeRequest struct {
	Action       any               `json:"action"`
	Nonce        uint64            `json:"nonce"`
	Signature    evmsign.Signature `json:"signature"`
	VaultAddress *string           `json:"vaultAddress"`
	ExpiresAfter *uint64           `json:"expiresAfter,omitempty"`
}

// ExchangeResponse is {"status":"ok","response":{...}} or
// {"status":"err","response":"message"}.
type ExchangeResponse struct {
	Status   string          `json:"status"`
	Response json.RawMessage `json:"response"`
	// Request is the signed payload, kept for logging and dry runs.
	Request ExchangeRequest `json:"-"`
}

func (r ExchangeResponse) err() error {
	if r.Status == "ok" || r.Status == "dry_run" {
		return nil
	}
	var msg string
	if json.Unmarshal(r.Response, &msg) != nil {
		msg = string(r.Response)
	}
	return fmt.Errorf("hyperliquid rejected action: %s", msg)
}

// Submit signs action and posts it. A non-ok venue status is an error.
func (c *ExchangeClient) Submit(ctx context.Context, action any) (ExchangeResponse, error) {
	p := L1Params{Nonce: c.nonces.Next(), VaultAddress: c.vault}
	sig, err := SignL1Action(c.signer, action, p, c.network)
	if err != nil {
		return ExchangeResponse{}, err
	}
	req := ExchangeRequest{Action: action, Nonce: p.Nonce, Signature: sig}
	if c.vault != nil {
		v := strings.ToLower(c.vault.Hex())
		req.VaultAddress = &v
	}
	if c.DryRun {
		return ExchangeResponse{Status: "dry_run", Request: req}, nil
	}
	var resp ExchangeResponse
	if err := c.t.post(ctx, "/exchange", req, &resp); err != nil {
		return ExchangeResponse{}, err
	}
	resp.Request = req
	return resp, resp.err()
}

// OrderStatusWire is one entry of response.data.statuses.
type OrderStatusWire struct {
	Resting *struct {
		Oid uint64 `json:"oid"`
	} `json:"resting,omitempty"`
	Filled *struct {
		TotalSz string `json:"totalSz"`
		AvgPx   string `json:"avgPx"`
		Oid     uint64 `json:"oid"`
	} `json:"filled,omitempty"`
	Error string `json:"error,omitempty"`
}

// OrderStatuses decodes the per-order statuses of an order action response.
func (r ExchangeResponse) OrderStatuses() ([]OrderStatusWire, error) {
	var body struct {
		Data struct {
			Statuses []OrderStatusWire `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.Response, &body); err != nil {
		return nil, fmt.Errorf("decode order statuses: %w", err)
	}
	return body.Data.Statuses, nil
}

// NonceSource hands out strictly increasing millisecond nonces. The venue
// rejects a reused nonce per signer, and two orders in the same millisecond
// would otherwise collide.
type NonceSource struct {
	mu   sync.Mutex
	last uint64
	now  func() time.Time
}

func NewNonceSource(now func() time.Time) *NonceSource {
	if now == nil {
		now = time.Now
	}
	return &NonceSource{now: now}
}

func (n *NonceSource) Next() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	next := uint64(n.now().UnixMilli())
	if next <= n.last {
		next = n.last + 1
	}
	n.last = next
	return next
}
