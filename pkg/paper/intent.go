package paper

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
)

// Action is the relayer route name for an intent.
type Action string

const (
	ActionRegisterSessionKey Action = "registerSessionKey"
	ActionOpenPosition       Action = "openPosition"
	ActionClosePositions     Action = "closePositions"
	ActionStake              Action = "paperStake"
	ActionUnstake            Action = "paperUnstake"
	ActionClaim              Action = "paperClaim"
)

// DefaultIntentTTL is the frontend's deadline. Shorter deadlines bound how
// long a queued intent can still execute after the caller gave up on it.
const DefaultIntentTTL = time.Hour

// intentIDTag prefixes every intent id: keccak(keccak(tag) ‖ digest ‖ signer).
var intentIDTag = crypto.Keccak256([]byte("papertrade:intent-id:v2"))

var eip712Domain = []apitypes.Type{
	{Name: "name", Type: "string"},
	{Name: "version", Type: "string"},
	{Name: "chainId", Type: "uint256"},
	{Name: "verifyingContract", Type: "address"},
}

// intentTypes are the frontend's field lists; their order is part of the
// signature.
var intentTypes = map[string][]apitypes.Type{
	"RegisterSessionKey": {{Name: "user", Type: "address"}, {Name: "key", Type: "address"}, {Name: "expiry", Type: "uint64"}, {Name: "feeAddress", Type: "address"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint64"}},
	"OpenPosition":       {{Name: "user", Type: "address"}, {Name: "instrumentId", Type: "uint32"}, {Name: "isLong", Type: "bool"}, {Name: "size", Type: "uint128"}, {Name: "leverage", Type: "uint32"}, {Name: "useDebt", Type: "bool"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint64"}},
	"Close":              {{Name: "user", Type: "address"}, {Name: "positionIds", Type: "uint256[]"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint64"}},
	"Stake":              {{Name: "user", Type: "address"}, {Name: "amount", Type: "uint256"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint64"}},
	"Unstake":            {{Name: "user", Type: "address"}, {Name: "amount", Type: "uint256"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint64"}},
	"Claim":              {{Name: "user", Type: "address"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint64"}},
}

// ExchangeDomain is the trading domain. The name really is "String": it was
// confirmed by recovering 113 of 113 on-chain OpenPosition intents.
func ExchangeDomain() apitypes.TypedDataDomain {
	return domain("String", ExchangeAddress)
}

// StakingDomain signs PAPER stake, unstake and claim intents.
func StakingDomain() apitypes.TypedDataDomain {
	return domain("PaperStaking", StakingAddress)
}

func domain(name string, contract common.Address) apitypes.TypedDataDomain {
	return apitypes.TypedDataDomain{
		Name: name, Version: "1", ChainId: math.NewHexOrDecimal256(ChainID),
		VerifyingContract: strings.ToLower(contract.Hex()),
	}
}

// TypedData assembles the EIP-712 payload for one intent type. Message
// values use the wire forms (decimal strings, lowercase addresses, bools).
func TypedData(dom apitypes.TypedDataDomain, primary string, message map[string]any) (apitypes.TypedData, error) {
	fields, ok := intentTypes[primary]
	if !ok {
		return apitypes.TypedData{}, fmt.Errorf("unknown paper intent type %q", primary)
	}
	return apitypes.TypedData{
		Types:       apitypes.Types{"EIP712Domain": eip712Domain, primary: fields},
		PrimaryType: primary,
		Domain:      dom,
		Message:     message,
	}, nil
}

// Intent is a signed relayer request. Signed is the exact wire object: every
// typed field in its wire form plus v ("27"/"28"), r and s.
type Intent struct {
	Action Action
	Signed map[string]any
	ID     common.Hash
	Digest common.Hash
	Signer common.Address
}

func (in Intent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Action Action         `json:"action"`
		Signed map[string]any `json:"signed"`
	}{in.Action, in.Signed})
}

// IntentID is the relayer's id for a digest signed by signer.
func IntentID(digest common.Hash, signer common.Address) common.Hash {
	return crypto.Keccak256Hash(intentIDTag, digest.Bytes(), signer.Bytes())
}

// SignIntent signs typed data and builds the wire intent.
func SignIntent(signer evmsign.Signer, action Action, td apitypes.TypedData) (Intent, error) {
	sig, err := signer.SignTypedData(td)
	if err != nil {
		return Intent{}, err
	}
	return AssembleIntent(action, td, sig)
}

// AssembleIntent attaches a signature produced elsewhere (for example the
// master wallet signing RegisterSessionKey in a browser wallet) and recovers
// the signer so the intent id matches what the relayer computes.
func AssembleIntent(action Action, td apitypes.TypedData, sig evmsign.Signature) (Intent, error) {
	digest, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		return Intent{}, fmt.Errorf("hash %s: %w", td.PrimaryType, err)
	}
	signer, err := evmsign.RecoverAddress(digest, sig)
	if err != nil {
		return Intent{}, err
	}
	signed := make(map[string]any, len(td.Message)+3)
	for _, f := range td.Types[td.PrimaryType] {
		v, ok := td.Message[f.Name]
		if !ok {
			return Intent{}, fmt.Errorf("%s missing field %s", td.PrimaryType, f.Name)
		}
		signed[f.Name] = v
	}
	signed["v"] = fmt.Sprint(sig.V)
	signed["r"] = strings.ToLower(sig.R)
	signed["s"] = strings.ToLower(sig.S)
	d := common.BytesToHash(digest)
	return Intent{Action: action, Signed: signed, ID: IntentID(d, signer), Digest: d, Signer: signer}, nil
}

// Intents builds and signs trading and staking intents for one account with
// its session key.
type Intents struct {
	user   common.Address
	signer evmsign.Signer
	nonces *Nonces
	ttl    time.Duration
	now    func() time.Time
}

// NewIntents signs for user with a registered session key. ttl <= 0 uses
// DefaultIntentTTL; nil now uses the wall clock.
func NewIntents(user common.Address, sessionKey evmsign.Signer, nonces *Nonces, ttl time.Duration, now func() time.Time) *Intents {
	if ttl <= 0 {
		ttl = DefaultIntentTTL
	}
	if now == nil {
		now = time.Now
	}
	return &Intents{user: user, signer: sessionKey, nonces: nonces, ttl: ttl, now: now}
}

func (b *Intents) User() common.Address { return b.user }

// Signer is the session key address the relayer must have registered for User.
func (b *Intents) Signer() common.Address { return b.signer.Address() }

func (b *Intents) base(group NonceGroup) (map[string]any, error) {
	nonce, err := b.nonces.Take(group)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"user":     wireAddress(b.user),
		"nonce":    nonce.String(),
		"deadline": fmt.Sprint(b.now().Add(b.ttl).Unix()),
	}, nil
}

// Open signs an OpenPosition. margin is in USD; notional = margin × leverage.
func (b *Intents) Open(in Instrument, isLong bool, margin *big.Int, leverage int) (Intent, error) {
	if leverage < 1 || leverage > MaxLeverage {
		return Intent{}, fmt.Errorf("paper leverage %d outside 1..%d", leverage, MaxLeverage)
	}
	if margin == nil || margin.Sign() <= 0 {
		return Intent{}, fmt.Errorf("paper margin must be positive")
	}
	msg, err := b.base(NonceExchange)
	if err != nil {
		return Intent{}, err
	}
	msg["instrumentId"] = fmt.Sprint(in.ID)
	msg["isLong"] = isLong
	msg["size"] = margin.String()
	msg["leverage"] = fmt.Sprint(leverage)
	msg["useDebt"] = false
	return b.sign(ActionOpenPosition, ExchangeDomain(), "OpenPosition", msg)
}

// Close signs one Close for every id, so all of them settle in the same block
// at the same price.
func (b *Intents) Close(positionIDs []string) (Intent, error) {
	if len(positionIDs) == 0 || len(positionIDs) > MaxCloseIDs {
		return Intent{}, fmt.Errorf("paper close takes 1..%d positions, got %d", MaxCloseIDs, len(positionIDs))
	}
	ids := make([]any, len(positionIDs))
	for i, id := range positionIDs {
		n, ok := new(big.Int).SetString(id, 10)
		if !ok || n.Sign() < 0 {
			return Intent{}, fmt.Errorf("paper position id %q is not a decimal integer", id)
		}
		ids[i] = n.String()
	}
	msg, err := b.base(NonceExchange)
	if err != nil {
		return Intent{}, err
	}
	msg["positionIds"] = ids
	return b.sign(ActionClosePositions, ExchangeDomain(), "Close", msg)
}

// Stake stakes amount raw PAPER.
func (b *Intents) Stake(amount *big.Int) (Intent, error) {
	return b.stakingAmount(ActionStake, "Stake", amount)
}

// Unstake unstakes amount raw PAPER.
func (b *Intents) Unstake(amount *big.Int) (Intent, error) {
	return b.stakingAmount(ActionUnstake, "Unstake", amount)
}

// Claim moves accrued USDC dividends to the trading balance.
func (b *Intents) Claim() (Intent, error) {
	msg, err := b.base(NonceStaking)
	if err != nil {
		return Intent{}, err
	}
	return b.sign(ActionClaim, StakingDomain(), "Claim", msg)
}

func (b *Intents) stakingAmount(action Action, primary string, amount *big.Int) (Intent, error) {
	if amount == nil || amount.Sign() <= 0 {
		return Intent{}, fmt.Errorf("paper %s amount must be positive", primary)
	}
	msg, err := b.base(NonceStaking)
	if err != nil {
		return Intent{}, err
	}
	msg["amount"] = amount.String()
	return b.sign(action, StakingDomain(), primary, msg)
}

func (b *Intents) sign(action Action, dom apitypes.TypedDataDomain, primary string, msg map[string]any) (Intent, error) {
	td, err := TypedData(dom, primary, msg)
	if err != nil {
		return Intent{}, err
	}
	return SignIntent(b.signer, action, td)
}

// RegisterSessionKeyTypedData is what the master wallet signs to authorize
// key for expiry. The frontend always sends the zero fee address.
func RegisterSessionKeyTypedData(user, key common.Address, expiry time.Time, nonce *big.Int, deadline time.Time) (apitypes.TypedData, error) {
	return TypedData(ExchangeDomain(), "RegisterSessionKey", map[string]any{
		"user":       wireAddress(user),
		"key":        wireAddress(key),
		"expiry":     fmt.Sprint(expiry.Unix()),
		"feeAddress": wireAddress(common.Address{}),
		"nonce":      nonce.String(),
		"deadline":   fmt.Sprint(deadline.Unix()),
	})
}

func wireAddress(a common.Address) string { return strings.ToLower(a.Hex()) }
