package hyperliquid

import (
	"fmt"

	"github.com/shopspring/decimal"
)

const (
	priceSigFigs    = 5
	perpMaxDecimals = 6
)

// RoundPerpPrice applies the venue tick rule for perps: at most 5 significant
// figures and at most (6 - szDecimals) decimals. The venue also accepts any
// integer price, but this rounds those to 5 significant figures too, exactly
// as the SDK does. Matches the SDK's round(float(f"{px:.5g}"), 6 - szDecimals).
func RoundPerpPrice(px decimal.Decimal, szDecimals int) decimal.Decimal {
	if px.IsZero() {
		return px
	}
	// floor(log10(|px|)) = digits(coefficient) + exponent - 1.
	magnitude := len(px.Coefficient().Abs(px.Coefficient()).String()) + int(px.Exponent()) - 1
	sig := px.RoundBank(int32(priceSigFigs - 1 - magnitude))
	return sig.RoundBank(int32(perpMaxDecimals - szDecimals))
}

// RoundSizeDown truncates a size to the asset's lot step so a hedge leg never
// exceeds the requested exposure.
func RoundSizeDown(sz decimal.Decimal, szDecimals int) decimal.Decimal {
	return sz.Truncate(int32(szDecimals))
}

// FloatToWire renders a decimal the way the venue hashes it: no exponent, no
// trailing zeros, at most 8 decimals. More precision is an error rather than a
// silent rounding.
func FloatToWire(d decimal.Decimal) (string, error) {
	if !d.Equal(d.Round(8)) {
		return "", fmt.Errorf("%s has more than 8 decimals", d)
	}
	if d.IsZero() {
		return "0", nil
	}
	return d.String(), nil
}
