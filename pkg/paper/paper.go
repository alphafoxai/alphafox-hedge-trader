// Package paper drives Papertrade (exchange.papertrade.xyz), a loss-mining
// perp on HyperEVM that settles every trade against its LP at the
// Hyperliquid BBO mid. Paper has no order book and no public API: trades are
// EIP-712 intents signed by a session key and posted to the official relayer,
// which batches them on chain.
//
// The wire format was reverse-engineered from the official frontend and
// checked against on-chain intents (see docs/paper-research.md); the golden
// tests pin it.
package paper

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
)

// ChainID is HyperEVM mainnet.
const ChainID = 999

const (
	DefaultRelayerURL = "https://exchange.papertrade.xyz"
	// DefaultBuild is the frontend build id sent as X-Papertrade-Build. It
	// changes on every frontend deploy; override it if the relayer rejects
	// stale builds.
	DefaultBuild = "7976669c6bab"
)

var (
	ExchangeAddress   = common.HexToAddress("0x6cd5661646289fb6e65ea5c032310fded797d0a2")
	StakingAddress    = common.HexToAddress("0xaad6c7b0cc3014fc80ffedae0ed7ce5967b0f016")
	TokenAddress      = common.HexToAddress("0xe40f17915daa230324030003a197cdaef2261c0e")
	TokenomicsAddress = common.HexToAddress("0x75174549dbbd44497d31a47e2e2d7406408d63d9")
)

const (
	MaxLeverage = 1000
	// MaxCloseIDs is the frontend's batch size for one Close intent; the
	// relayer validator accepts up to 25.
	MaxCloseIDs = 12
)

// Instrument is one Paper market. PriceScale converts raw prices:
// price = raw / PriceScale.
type Instrument struct {
	ID         uint32
	Symbol     string
	PriceScale int64
}

var instruments = []Instrument{
	{ID: 0, Symbol: "BTC", PriceScale: 10},
	{ID: 1, Symbol: "ETH", PriceScale: 100},
}

func InstrumentBySymbol(symbol string) (Instrument, error) {
	for _, in := range instruments {
		if in.Symbol == symbol {
			return in, nil
		}
	}
	return Instrument{}, fmt.Errorf("paper has no instrument %q (BTC, ETH)", symbol)
}

func InstrumentByID(id uint32) (Instrument, error) {
	for _, in := range instruments {
		if in.ID == id {
			return in, nil
		}
	}
	return Instrument{}, fmt.Errorf("paper has no instrument id %d", id)
}

// Price converts a raw price string.
func (in Instrument) Price(raw string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("raw price %q: %w", raw, err)
	}
	return d.Div(decimal.NewFromInt(in.PriceScale)), nil
}

// USD amounts (margin, balances, PnL) and PAPER amounts are 18-decimal raw
// integers on the wire.
var unit = decimal.New(1, 18)

// ToRaw converts a USD or PAPER amount to its raw integer, truncating dust.
func ToRaw(v decimal.Decimal) *big.Int { return v.Mul(unit).Truncate(0).BigInt() }

// FromRaw parses a raw 18-decimal integer string; empty means zero.
func FromRaw(raw string) (decimal.Decimal, error) {
	if raw == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("raw amount %q: %w", raw, err)
	}
	return d.Shift(-18), nil
}
