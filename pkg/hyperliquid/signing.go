package hyperliquid

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
)

// Network selects the venue chain. There is no default: an order signed for
// the wrong chain is silently rejected or, worse, accepted on the wrong one.
type Network string

const (
	Mainnet Network = "mainnet"
	Testnet Network = "testnet"
)

func ParseNetwork(s string) (Network, error) {
	switch Network(s) {
	case Mainnet, Testnet:
		return Network(s), nil
	}
	return "", fmt.Errorf("hyperliquid network must be %q or %q, got %q", Mainnet, Testnet, s)
}

func (n Network) APIBaseURL() string {
	if n == Mainnet {
		return "https://api.hyperliquid.xyz"
	}
	return "https://api.hyperliquid-testnet.xyz"
}

// agentSource is the phantom-agent source marker: mainnet signs "a", testnet "b".
func (n Network) agentSource() string {
	if n == Mainnet {
		return "a"
	}
	return "b"
}

// L1Params are the non-action inputs to an L1 action hash.
type L1Params struct {
	Nonce uint64
	// VaultAddress trades on behalf of a vault or subaccount; nil for the
	// signer's own account (agent wallets sign for their master this way).
	VaultAddress *common.Address
	// ExpiresAfter (ms) makes the venue reject the action after that time.
	ExpiresAfter *uint64
}

// ActionHash generalizes the setReferrer-only hash in alphafox-engine
// walletsigner: keccak(msgpack(action) || nonce u64 BE || vault marker
// [|| 0x00 || expiresAfter u64 BE]).
func ActionHash(action any, p L1Params) ([]byte, error) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	// The SDK packs integers in their smallest form; msgpack-go defaults to
	// fixed-width int64, which would change the hash.
	enc.UseCompactInts(true)
	if err := enc.Encode(action); err != nil {
		return nil, fmt.Errorf("msgpack action: %w", err)
	}
	var word [8]byte
	binary.BigEndian.PutUint64(word[:], p.Nonce)
	buf.Write(word[:])
	if p.VaultAddress == nil {
		buf.WriteByte(0)
	} else {
		buf.WriteByte(1)
		buf.Write(p.VaultAddress.Bytes())
	}
	if p.ExpiresAfter != nil {
		buf.WriteByte(0)
		binary.BigEndian.PutUint64(word[:], *p.ExpiresAfter)
		buf.Write(word[:])
	}
	return crypto.Keccak256(buf.Bytes()), nil
}

var agentTypes = apitypes.Types{
	"EIP712Domain": {
		{Name: "name", Type: "string"},
		{Name: "version", Type: "string"},
		{Name: "chainId", Type: "uint256"},
		{Name: "verifyingContract", Type: "address"},
	},
	"Agent": {
		{Name: "source", Type: "string"},
		{Name: "connectionId", Type: "bytes32"},
	},
}

// L1TypedData wraps an action hash in the phantom Agent struct that L1
// actions sign.
func L1TypedData(actionHash []byte, network Network) apitypes.TypedData {
	return apitypes.TypedData{
		Types:       agentTypes,
		PrimaryType: "Agent",
		Domain: apitypes.TypedDataDomain{
			Name:              "Exchange",
			Version:           "1",
			ChainId:           math.NewHexOrDecimal256(1337),
			VerifyingContract: "0x0000000000000000000000000000000000000000",
		},
		Message: apitypes.TypedDataMessage{
			"source":       network.agentSource(),
			"connectionId": hexutil.Encode(actionHash),
		},
	}
}

func SignL1Action(signer evmsign.Signer, action any, p L1Params, network Network) (evmsign.Signature, error) {
	hash, err := ActionHash(action, p)
	if err != nil {
		return evmsign.Signature{}, err
	}
	return signer.SignTypedData(L1TypedData(hash, network))
}
