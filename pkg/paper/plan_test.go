package paper

import (
	"math"
	"strings"
	"testing"
)

func TestRealizedVol(t *testing.T) {
	// Alternating ±0.1% per minute: σ = 0.1% × √60 per √hour.
	closes := []float64{100}
	for i := 0; i < 600; i++ {
		f := 1.001
		if i%2 == 1 {
			f = 1 / 1.001
		}
		closes = append(closes, closes[len(closes)-1]*f)
	}
	near(t, "σ", RealizedVol(closes, 60), math.Log(1.001)*math.Sqrt(60), 1e-12)
	if RealizedVol([]float64{100}, 60) != 0 || RealizedVol([]float64{100, 0, 101}, 60) != 0 {
		t.Error("degenerate series must give 0")
	}
	near(t, "E[T]", ExpectedHours(0.005, 0.0035), 2.0408, 0.0001)
}

func btcPlan(capital, hours float64) Plan {
	return Recommend(PlanInput{Symbol: "BTC", Impact: LaunchImpact["BTC"], MaxLeverage: 1000, MinMargin: 10,
		MinNotional: 10_000, MintRate: 36.6, QueueEmpty: true, Capital: capital, Sigma: 0.0034, TargetHours: hours})
}

func TestRecommendRespectsVenueAndBust(t *testing.T) {
	for _, c := range []float64{60, 200, 500, 5000} {
		for _, h := range []float64{0.25, 1, 2, 8} {
			p := btcPlan(c, h)
			if !p.Feasible {
				t.Fatalf("cap %v wait %v infeasible: %s", c, h, p.Reason)
			}
			if p.Margin < 10 || p.Notional < 10_000 || p.Leverage > 1000 || p.Margin*2 > c+1e-9 {
				t.Errorf("cap %v: venue limits broken %+v", c, p)
			}
			if p.CloseMove+p.BustGuard > p.BustDistance+1e-12 || p.BustGuard < 0.0005 {
				t.Errorf("cap %v wait %v: move %v + guard %v exceeds bust %v", c, h, p.CloseMove, p.BustGuard, p.BustDistance)
			}
			if p.ExpectedHours > h*1.0001 {
				t.Errorf("cap %v: wait %v over target %v", c, p.ExpectedHours, h)
			}
		}
	}
}

func TestRecommendTradesWaitForCost(t *testing.T) {
	fast, slow := btcPlan(500, 0.5), btcPlan(500, 2)
	if !(slow.CloseMove > fast.CloseMove && slow.Estimate.CostPerPaper < fast.CostPerHour && slow.Estimate.CostPerPaper < fast.Estimate.CostPerPaper) {
		t.Fatalf("longer wait should mean a larger move and cheaper PAPER: fast %+v slow %+v", fast, slow)
	}
	if slow.Leverage >= fast.Leverage {
		t.Fatalf("a larger move needs lower leverage: %d vs %d", slow.Leverage, fast.Leverage)
	}
}

func TestRecommendSmallCapital(t *testing.T) {
	p := btcPlan(20, 2)
	if p.Feasible || !strings.Contains(p.Reason, "no room") || p.CapitalForPlan <= 20 {
		t.Fatalf("20U at 1000x cannot fit a 0.05%% guard: %+v", p)
	}
	if p := btcPlan(15, 2); p.Feasible || p.CapitalForPlan != 20 {
		t.Fatalf("below two minimum margins: %+v", p)
	}
	capped := btcPlan(60, 2)
	if !capped.Feasible || !strings.Contains(capped.Reason, "capped") || capped.CapitalForTarget <= 60 {
		t.Fatalf("60U should run capped by bust distance: %+v", capped)
	}
}
