package paper

import (
	"math"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f ± %g", name, got, want, tol)
	}
}

// The docs: on BTC, counting the deadband, a 1% move keeps about 88% of the
// raw gain and a 0.1% move about 74% (before the 2% win fee).
func TestImpactMatchesDocs(t *testing.T) {
	btc := LaunchImpact["BTC"]
	near(t, "keep@0.1%", btc.WinningClose(1, 0.001).Gross/0.001, 0.74, 0.005)
	near(t, "keep@1%", btc.WinningClose(1, 0.01).Gross/0.01, 0.88, 0.005)
	near(t, "net@0.1%", btc.WinningClose(1, 0.001).Net/0.001, 0.724, 0.001)
	near(t, "net@1%", btc.WinningClose(1, 0.01).Net/0.01, 0.864, 0.001)
	if w := btc.WinningClose(1e6, Deadband/2); w.Net != 0 {
		t.Errorf("a win inside the deadband paid %v", w.Net)
	}
}

// A real same-block long+short close (tx 0x1f91865e…, BTC 200x, margin
// 6128.83 per leg, entry 83072.5, exit 82969.5): short adjustedPnl 1165.676,
// fee 23.3135; long lost 1519.804 and minted 54709.71 PAPER on basis 1489.408.
func TestEstimateMatchesOnChainPair(t *testing.T) {
	notional := 6128.830578 * 200
	move := (83072.5 - 82969.5) / 83072.5
	win := LaunchImpact["BTC"].WinningClose(notional, move)
	near(t, "gross", win.Gross, 1165.676, 1165.676*0.0025) // within 0.25%
	near(t, "fee", win.Fee, 23.3135, 23.3135*0.0025)

	rate := 54709.706581921589294835 / 1489.407827002485780493
	est := EstimatePair(LaunchImpact["BTC"], notional, move, rate, true)
	near(t, "loss", est.Loss, 1519.804, 0.01)
	near(t, "paper", est.PaperMinted, 54709.71, 1)
	near(t, "costPerPaper", est.CostPerPaper, (1519.804-(1165.676-23.3135))/54709.71, 0.00005)
}

func TestMintRate(t *testing.T) {
	near(t, "flat", MintRate(100, 120e6, 0), 100, 1e-9)
	// tailProgressUsd read on chain 2026-10-11.
	near(t, "now", MintRate(100, 120e6, 77_876_520), 36.78, 0.01)
	near(t, "H=100M", MintRate(100, 120e6, 100e6), 29.75, 0.01)
}

// The research table: 10,000 notional per leg on BTC, mint rate 36.78.
func TestPairCostTable(t *testing.T) {
	for _, c := range []struct{ move, net, paper, cpp float64 }{
		{0.001, 2.76, 360, 0.0077},
		{0.002, 4.06, 721, 0.0056},
		{0.005, 7.69, 1802, 0.0043},
		{0.008, 11.25, 2883, 0.0039},
	} {
		e := EstimatePair(LaunchImpact["BTC"], 10_000, c.move, 36.78, true)
		near(t, "netCost", e.NetCost, c.net, 0.01)
		near(t, "paper", e.PaperMinted, c.paper, 1)
		near(t, "costPerPaper", e.CostPerPaper, c.cpp, 0.00006)
	}
}

func TestMaxSafeMove(t *testing.T) {
	near(t, "100x", MaxSafeMove(100), 0.0095, 1e-12)
	near(t, "1000x", MaxSafeMove(1000), 0.0005, 1e-12)
	if MaxSafeMove(5000) != 0 {
		t.Error("leverage above 2000x has no safe move")
	}
}
