// Package evmsign holds the secp256k1 / EIP-712 primitives shared by every
// venue adapter. It is ported from alphafox-engine pkg/walletsigner (sign.go)
// without the custody, KMS, and storage layers.
package evmsign

import (
	"crypto/ecdsa"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// Signature is the split r/s/v form venues expect on the wire.
type Signature struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

// Signer signs EIP-712 typed data with one key. Venue adapters depend on this
// interface rather than on a raw key so a custody-backed signer can replace
// LocalSigner later without touching order code.
type Signer interface {
	Address() common.Address
	SignTypedData(data apitypes.TypedData) (Signature, error)
}

// LocalSigner keeps a private key in process memory. Use it only with an
// agent (API) wallet key, never with a main wallet that holds funds.
type LocalSigner struct {
	key     *ecdsa.PrivateKey
	address common.Address
}

// NewLocalSigner parses a 0x-prefixed or bare hex private key.
func NewLocalSigner(hexKey string) (*LocalSigner, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(hexKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	return &LocalSigner{key: key, address: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

func (s *LocalSigner) Address() common.Address { return s.address }

func (s *LocalSigner) SignTypedData(data apitypes.TypedData) (Signature, error) {
	digest, _, err := apitypes.TypedDataAndHash(data)
	if err != nil {
		return Signature{}, fmt.Errorf("hash typed data: %w", err)
	}
	return SignDigest(digest, s.key)
}

// SignDigest signs a 32-byte digest and returns V as 27 or 28.
func SignDigest(digest []byte, key *ecdsa.PrivateKey) (Signature, error) {
	if len(digest) != 32 {
		return Signature{}, fmt.Errorf("digest must be 32 bytes, got %d", len(digest))
	}
	sig, err := crypto.Sign(digest, key)
	if err != nil {
		return Signature{}, fmt.Errorf("sign digest: %w", err)
	}
	return Signature{
		R: hexWord(sig[:32]),
		S: hexWord(sig[32:64]),
		V: int(sig[64]) + 27,
	}, nil
}

// RecoverAddress returns the signer of digest; tests use it to check a
// signature without comparing r/s encodings.
func RecoverAddress(digest []byte, sig Signature) (common.Address, error) {
	raw := make([]byte, 0, 65)
	raw = append(raw, common.FromHex(sig.R)...)
	raw = append(raw, common.FromHex(sig.S)...)
	if len(raw) != 64 || (sig.V != 27 && sig.V != 28) {
		return common.Address{}, fmt.Errorf("malformed signature")
	}
	raw = append(raw, byte(sig.V-27))
	pub, err := crypto.SigToPub(digest, raw)
	if err != nil {
		return common.Address{}, fmt.Errorf("recover public key: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}

func hexWord(b []byte) string {
	return common.BytesToHash(b).Hex()
}
