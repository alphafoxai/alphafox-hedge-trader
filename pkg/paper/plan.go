package paper

import (
	"math"
	"sort"
)

// RealizedVol is the volatility per √hour of a close series sampled every
// periodSeconds: the root mean square of log returns, scaled by √(3600/period).
// Mean is not removed; over minutes it is noise.
func RealizedVol(closes []float64, periodSeconds float64) float64 {
	var sum float64
	n := 0
	for i := 1; i < len(closes); i++ {
		if closes[i-1] <= 0 || closes[i] <= 0 {
			continue
		}
		r := math.Log(closes[i] / closes[i-1])
		sum += r * r
		n++
	}
	if n == 0 || periodSeconds <= 0 {
		return 0
	}
	return math.Sqrt(sum/float64(n)) * math.Sqrt(3600/periodSeconds)
}

// ExpectedHours is the mean time for a driftless price with volatility
// sigma per √hour to first move ±move from where it started: move²/σ².
func ExpectedHours(move, sigma float64) float64 {
	if sigma <= 0 {
		return math.Inf(1)
	}
	return (move / sigma) * (move / sigma)
}

// PlanInput is what a recommendation is computed from.
type PlanInput struct {
	Symbol          string
	Impact          ImpactParams
	MaxLeverage     int
	MinMargin       float64
	MinNotional     float64
	MintRate        float64
	QueueEmpty      bool
	Capital         float64 // USD available to this symbol; both legs share it
	CapitalUse      float64 // share of Capital to post as margin; 0 means all of it
	Sigma           float64 // realized volatility per √hour
	TargetHours     float64 // acceptable mean wait for a trigger; 0 means 2h
	CloseLatency    float64 // seconds from trigger to settled close; 0 means 10s
	GuardSigmas     float64 // bust guard in σ over CloseLatency; 0 means 6
	MinGuard        float64 // floor on the bust guard; 0 means 0.0005
	MaxGuardOfMove  float64 // guard never needs to exceed this share of the move; 0 means 0.5
	LeverageChoices []int   // leverages to consider; nil means common steps up to MaxLeverage
}

// Plan is one recommended setting.
type Plan struct {
	Symbol        string       `json:"symbol"`
	Feasible      bool         `json:"feasible"`
	Reason        string       `json:"reason,omitempty"`
	Leverage      int          `json:"leverage"`
	Margin        float64      `json:"marginPerLeg"`
	Notional      float64      `json:"notionalPerLeg"`
	CloseMove     float64      `json:"closeMove"`
	BustGuard     float64      `json:"bustGuard"`
	BustDistance  float64      `json:"bustDistance"`
	Sigma         float64      `json:"sigmaPerSqrtHour"`
	ExpectedHours float64      `json:"expectedHours"`
	Estimate      PairEstimate `json:"estimate"`
	PaperPerHour  float64      `json:"paperPerHour"`
	CostPerHour   float64      `json:"costPerHour"`
	// CapitalForPlan is the least capital that runs this plan at the minimum
	// notional; CapitalForTarget is what the target-wait move would need.
	CapitalForPlan   float64 `json:"capitalForPlan"`
	CapitalForTarget float64 `json:"capitalForTarget"`
}

var defaultLeverages = []int{2, 3, 5, 10, 15, 20, 25, 30, 40, 50, 60, 75, 100, 125, 150, 200, 250, 300, 400, 500, 600, 750, 1000}

// Recommend picks the leverage and close move that minimise cost per PAPER
// for the capital at hand.
//
// Cost per PAPER falls as the close move grows, so the move is as large as
// the wait budget (TargetHours at the current σ) and the bust distance allow.
// The bust guard covers the close latency at GuardSigmas. Given the move, the
// highest safe leverage maximises notional and so PAPER per cycle. The
// venue's minimum notional caps how low leverage can go for small capital.
func Recommend(in PlanInput) Plan {
	in = in.withDefaults()
	p := Plan{Symbol: in.Symbol, Sigma: in.Sigma}
	if in.Sigma <= 0 {
		p.Reason = "no volatility estimate"
		return p
	}
	margin := in.Capital * in.CapitalUse / 2
	targetMove := in.Sigma * math.Sqrt(in.TargetHours)
	guardFor := func(move float64) float64 {
		g := in.GuardSigmas * in.Sigma * math.Sqrt(in.CloseLatency/3600)
		g = math.Max(g, in.MinGuard)
		return math.Min(g, math.Max(in.MinGuard, move*in.MaxGuardOfMove))
	}
	targetGuard := guardFor(targetMove)
	p.CapitalForTarget = 2 * in.MinNotional / float64(maxLeverageFor(in, targetMove+targetGuard)) / in.CapitalUse

	if margin < in.MinMargin {
		p.Reason = "capital below two minimum margins"
		p.CapitalForPlan = 2 * in.MinMargin / in.CapitalUse
		return p
	}
	// The lowest leverage this capital may use, then the room it leaves.
	floor := 0
	for _, l := range in.LeverageChoices {
		if l <= in.MaxLeverage && margin*float64(l) >= in.MinNotional {
			floor = l
			break
		}
	}
	if floor == 0 {
		p.Reason = "capital too small for the minimum notional at max leverage"
		p.CapitalForPlan = 2 * in.MinNotional / float64(in.MaxLeverage) / in.CapitalUse
		return p
	}
	move := targetMove
	if room := MaxSafeMove(floor); move+guardFor(move) > room {
		// Shrink to fit: solve move + guard(move) = room on the guard's pieces.
		move = room - guardFor(room)
		if g := guardFor(move); move+g > room {
			move = room - g
		}
	}
	if move <= Deadband {
		p.Leverage, p.BustDistance = floor, MaxSafeMove(floor)
		p.Reason = "no room between the close move and the bust guard at the leverage this capital needs"
		p.CapitalForPlan = p.CapitalForTarget
		return p
	}
	guard := guardFor(move)
	lev := maxLeverageFor(in, move+guard)
	if lev < floor {
		lev = floor
	}
	p.Feasible = true
	p.Leverage, p.Margin = lev, margin
	p.Notional = margin * float64(lev)
	p.CloseMove, p.BustGuard, p.BustDistance = move, guard, MaxSafeMove(lev)
	p.ExpectedHours = ExpectedHours(move, in.Sigma)
	p.Estimate = EstimatePair(in.Impact, p.Notional, move, in.MintRate, in.QueueEmpty)
	if p.ExpectedHours > 0 {
		p.PaperPerHour = p.Estimate.PaperMinted / p.ExpectedHours
		p.CostPerHour = p.Estimate.NetCost / p.ExpectedHours
	}
	p.CapitalForPlan = 2 * in.MinNotional / float64(lev) / in.CapitalUse
	if move < targetMove*0.999 {
		p.Reason = "close move capped by the bust distance; more capital allows lower leverage and a larger move"
	}
	return p
}

// maxLeverageFor is the highest offered leverage whose bust distance exceeds
// need, or the lowest offered when none does.
func maxLeverageFor(in PlanInput, need float64) int {
	best := in.LeverageChoices[0]
	for _, l := range in.LeverageChoices {
		if l <= in.MaxLeverage && MaxSafeMove(l) >= need {
			best = l
		}
	}
	return best
}

func (in PlanInput) withDefaults() PlanInput {
	if in.CapitalUse <= 0 || in.CapitalUse > 1 {
		in.CapitalUse = 1
	}
	if in.TargetHours <= 0 {
		in.TargetHours = 2
	}
	if in.CloseLatency <= 0 {
		in.CloseLatency = 10
	}
	if in.GuardSigmas <= 0 {
		in.GuardSigmas = 6
	}
	if in.MinGuard <= 0 {
		in.MinGuard = 0.0005
	}
	if in.MaxGuardOfMove <= 0 {
		in.MaxGuardOfMove = 0.5
	}
	if in.MaxLeverage <= 0 {
		in.MaxLeverage = MaxLeverage
	}
	if len(in.LeverageChoices) == 0 {
		in.LeverageChoices = defaultLeverages
	}
	in.LeverageChoices = append([]int(nil), in.LeverageChoices...)
	sort.Ints(in.LeverageChoices)
	return in
}
