package paper

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
)

// Golden intents decoded from BatchExecutor calldata on HyperEVM mainnet. The
// intent id is what the relayer put in the batch, so matching it proves the
// domain, type strings, signer recovery and id derivation all agree.
var goldenIntents = []struct {
	name, primary, intentID, signer string
	action                          Action
	message                         map[string]any
	sig                             evmsign.Signature
}{
	{
		name: "open", primary: "OpenPosition", action: ActionOpenPosition,
		// tx 0xb904570074b6041f9c5a480e5275e46799a5297bf2e03ff703daba5bd8c770b5
		intentID: "0xbace16ff27a5a83d992944c0c3ee2954e7d7ef548e58642b002045ad638cdb92",
		signer:   "0xD8a842B0e79A7d219f943bD0427C0C8b50Af724b",
		message: map[string]any{
			"user": "0x0a1d4262a3d1a40c45267ec10931da18591040ec", "instrumentId": "1", "isLong": true,
			"size": "10000000000000000000", "leverage": "1000", "useDebt": false,
			"nonce": "2114127130", "deadline": "1791708420",
		},
		sig: evmsign.Signature{V: 27,
			R: "0x4bff5e1cf7ca3e2bcaed241b1f503e41d5544fd24114847d68dfca1468a9f963",
			S: "0x2b459149f3fa8308dae98cb66e56e198436fa6c663756be7bb0981d3bc3a7f71"},
	},
	{
		name: "close", primary: "Close", action: ActionClosePositions,
		// tx 0x0f13b41fabcf9681b2754c579c06553b2b8509c4157a19c8a7fd1baa2d39ded5
		intentID: "0xd82b4ebec035da45f2991a52fe97b859375577c026eea9945bc2efd165aa8682",
		message: map[string]any{
			"user": "0xacadbd12c0eb153bb3e56f337eb34850325a6cec", "positionIds": []any{"131172", "131173"},
			"nonce": "117421168591569291", "deadline": "1791791243",
		},
		sig: evmsign.Signature{V: 28,
			R: "0xa69043f2e00dc2c210f93c3015bc31746c0500b251dab96006e8059d5a6247c6",
			S: "0x507e324148af799e6d6176bacc02b03635e8fd82f9f0f98640a9b8e0c0bc1283"},
	},
}

func TestGoldenOnChainIntents(t *testing.T) {
	for _, g := range goldenIntents {
		t.Run(g.name, func(t *testing.T) {
			td, err := TypedData(ExchangeDomain(), g.primary, g.message)
			if err != nil {
				t.Fatal(err)
			}
			in, err := AssembleIntent(g.action, td, g.sig)
			if err != nil {
				t.Fatal(err)
			}
			if in.ID.Hex() != g.intentID {
				t.Fatalf("intent id %s, want %s (signer %s)", in.ID.Hex(), g.intentID, in.Signer.Hex())
			}
			if g.signer != "" && in.Signer != common.HexToAddress(g.signer) {
				t.Fatalf("signer %s, want %s", in.Signer.Hex(), g.signer)
			}
		})
	}
}

// Test key from go-ethereum's docs; it holds nothing.
const testKey = "b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291"

func fixedClock() time.Time { return time.Unix(1_800_000_000, 0) }

func newTestIntents(t *testing.T) (*Intents, *evmsign.LocalSigner) {
	t.Helper()
	key, err := evmsign.NewLocalSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	// Seed bytes 0x000102 → nonce start 0x000102 << 8.
	nonces := NewNonces(bytes.NewReader(bytes.Repeat([]byte{0, 1, 2}, 4)))
	user := common.HexToAddress("0x00000000000000000000000000000000000000AB")
	return NewIntents(user, key, nonces, 2*time.Minute, fixedClock), key
}

func TestOpenIntentWireForm(t *testing.T) {
	b, key := newTestIntents(t)
	in, err := b.Open(Instrument{ID: 0, Symbol: "BTC", PriceScale: 10}, false, big.NewInt(10_000_000_000_000_000), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if in.Signer != key.Address() {
		t.Fatalf("signer %s, want session key %s", in.Signer.Hex(), key.Address().Hex())
	}
	if in.ID != IntentID(in.Digest, key.Address()) {
		t.Fatal("intent id does not derive from digest and signer")
	}
	want := map[string]any{
		"user": "0x00000000000000000000000000000000000000ab", "instrumentId": "0", "isLong": false,
		"size": "10000000000000000", "leverage": "1000", "useDebt": false,
		"nonce": "66048", "deadline": "1800000120", "v": in.Signed["v"],
		"r": in.Signed["r"], "s": in.Signed["s"],
	}
	for k, v := range want {
		if in.Signed[k] != v {
			t.Errorf("signed[%s] = %#v, want %#v", k, in.Signed[k], v)
		}
	}
	if len(in.Signed) != len(want) {
		t.Errorf("signed has %d fields, want %d", len(in.Signed), len(want))
	}
	if v := in.Signed["v"]; v != "27" && v != "28" {
		t.Errorf("v = %#v, want \"27\" or \"28\"", v)
	}
	if r := in.Signed["r"].(string); len(r) != 66 || r != strings.ToLower(r) {
		t.Errorf("r = %q, want lowercase 32-byte hex", r)
	}

	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Action string         `json:"action"`
		Signed map[string]any `json:"signed"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Action != "openPosition" || wire.Signed["size"] != "10000000000000000" {
		t.Fatalf("wire body %s (%v)", body, err)
	}

	// The next exchange intent takes the next nonce.
	next, err := b.Close([]string{"7"})
	if err != nil {
		t.Fatal(err)
	}
	if next.Signed["nonce"] != "66049" {
		t.Fatalf("second nonce %v, want 66049", next.Signed["nonce"])
	}
}

func TestCloseIntentValidation(t *testing.T) {
	b, _ := newTestIntents(t)
	in, err := b.Close([]string{"131172", "131173"})
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := in.Signed["positionIds"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "131172" || ids[1] != "131173" {
		t.Fatalf("positionIds = %#v", in.Signed["positionIds"])
	}
	tooMany := make([]string, MaxCloseIDs+1)
	for i := range tooMany {
		tooMany[i] = "1"
	}
	for _, bad := range [][]string{nil, tooMany, {"0x10"}, {"-1"}} {
		if _, err := b.Close(bad); err == nil {
			t.Errorf("Close(%v) accepted", bad)
		}
	}
	if _, err := b.Open(Instrument{}, true, big.NewInt(1), MaxLeverage+1); err == nil {
		t.Error("Open accepted leverage above the cap")
	}
	if _, err := b.Open(Instrument{}, true, big.NewInt(0), 10); err == nil {
		t.Error("Open accepted zero margin")
	}
}

func TestStakingIntentsUseStakingDomainAndNonces(t *testing.T) {
	b, key := newTestIntents(t)
	stake, err := b.Stake(big.NewInt(5))
	if err != nil {
		t.Fatal(err)
	}
	td, _ := TypedData(StakingDomain(), "Stake", map[string]any{
		"user": stake.Signed["user"], "amount": "5", "nonce": stake.Signed["nonce"], "deadline": stake.Signed["deadline"],
	})
	digest, _, err := apitypes.TypedDataAndHash(td)
	if err != nil {
		t.Fatal(err)
	}
	if common.BytesToHash(digest) != stake.Digest || stake.Signer != key.Address() {
		t.Fatal("stake intent is not signed over the PaperStaking domain")
	}
	claim, err := b.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if stake.Signed["nonce"] != "66048" || claim.Signed["nonce"] != "66049" {
		t.Fatalf("staking nonces %v, %v; want their own sequence", stake.Signed["nonce"], claim.Signed["nonce"])
	}
	if _, err := b.Unstake(big.NewInt(0)); err == nil {
		t.Error("Unstake accepted zero")
	}
}

func TestRegisterSessionKeySignedByMaster(t *testing.T) {
	master, err := evmsign.NewLocalSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	session := common.HexToAddress("0x1111111111111111111111111111111111111111")
	td, err := RegisterSessionKeyTypedData(master.Address(), session, fixedClock().Add(30*24*time.Hour), big.NewInt(256), fixedClock().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if td.Message["feeAddress"] != "0x0000000000000000000000000000000000000000" {
		t.Fatalf("feeAddress %v, want zero", td.Message["feeAddress"])
	}
	sig, err := master.SignTypedData(td)
	if err != nil {
		t.Fatal(err)
	}
	in, err := AssembleIntent(ActionRegisterSessionKey, td, sig)
	if err != nil {
		t.Fatal(err)
	}
	if in.Signer != master.Address() || in.Signed["key"] != "0x1111111111111111111111111111111111111111" {
		t.Fatalf("register intent signer %s key %v", in.Signer.Hex(), in.Signed["key"])
	}
}

func TestNoncesSeedAndIncrement(t *testing.T) {
	n := NewNonces(nil)
	a, err := n.Take(NonceExchange)
	if err != nil {
		t.Fatal(err)
	}
	if new(big.Int).Mod(a, big.NewInt(256)).Sign() != 0 || a.Cmp(big.NewInt(1<<32)) >= 0 {
		t.Fatalf("seed %s is not a 24-bit value × 256", a)
	}
	b, _ := n.Take(NonceExchange)
	if new(big.Int).Sub(b, a).Int64() != 1 {
		t.Fatalf("nonces %s then %s, want +1", a, b)
	}
}
