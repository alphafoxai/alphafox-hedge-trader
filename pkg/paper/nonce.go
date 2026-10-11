package paper

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"sync"
)

// NonceGroup is a verifying contract; each keeps its own nonce sequence.
type NonceGroup string

const (
	NonceExchange NonceGroup = "exchange"
	NonceStaking  NonceGroup = "staking"
)

// Nonces hands out client-chosen intent nonces the way the frontend does: a
// random 24-bit start × 256, then +1 per intent. Nonces only need to be
// unused, so a restart re-seeds instead of persisting a counter.
type Nonces struct {
	mu   sync.Mutex
	rand io.Reader
	next map[NonceGroup]*big.Int
}

// NewNonces seeds from r; nil uses crypto/rand.
func NewNonces(r io.Reader) *Nonces {
	if r == nil {
		r = rand.Reader
	}
	return &Nonces{rand: r, next: map[NonceGroup]*big.Int{}}
}

func (n *Nonces) Take(group NonceGroup) (*big.Int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cur, ok := n.next[group]
	if !ok {
		seed, err := rand.Int(n.rand, big.NewInt(1<<24))
		if err != nil {
			return nil, fmt.Errorf("seed paper nonce: %w", err)
		}
		cur = seed.Lsh(seed, 8)
	}
	n.next[group] = new(big.Int).Add(cur, big.NewInt(1))
	return new(big.Int).Set(cur), nil
}
