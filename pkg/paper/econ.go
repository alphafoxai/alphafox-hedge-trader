package paper

import "math"

// Settlement constants from the docs, checked against on-chain closes.
const (
	// Deadband: a winning exit is measured as if it were 0.2 bp worse.
	Deadband = 1.0 / 50000
	WinFee   = 0.02
	// LossMintFactor: while the payout queue is empty, PAPER is minted on the
	// loss net of the 2% fee carved for the LP.
	LossMintFactor = 0.98
	// BustBuffer approximates how far before zero equity the hard bust sits;
	// positions report their exact bust price once open.
	BustBuffer = 0.0005
)

// ImpactParams are an instrument's asymmetric-impact parameters.
type ImpactParams struct {
	BaseRate           float64
	RateMultiplier     float64
	PositionMultiplier float64
	ReferenceNotional  float64
}

// LaunchImpact holds the on-chain values at launch; prefer the live values
// from Client.Trading.
var LaunchImpact = map[string]ImpactParams{
	"BTC": {BaseRate: 0.1, RateMultiplier: 15000, PositionMultiplier: 814.598, ReferenceNotional: 100000},
	"ETH": {BaseRate: 0.1, RateMultiplier: 15000, PositionMultiplier: 483.979, ReferenceNotional: 100000},
}

// Scale is the share of the raw gain a winner keeps at a move (fraction,
// after the deadband), before the win fee.
func (p ImpactParams) Scale(move float64) float64 {
	if move <= 0 {
		return 0
	}
	term1 := 1 / (move * p.RateMultiplier)
	term2 := p.ReferenceNotional / (1e6 * move * p.PositionMultiplier)
	return (1 - p.BaseRate) / (1 + term1 + term2)
}

// Win is a winning close: Gross after impact, Fee the 2% win fee, Net what
// the trader is credited.
type Win struct {
	Gross, Fee, Net float64
}

// WinningClose estimates a winning close of notional at a raw price move.
func (p ImpactParams) WinningClose(notional, move float64) Win {
	m := move - Deadband
	if m <= 0 {
		return Win{}
	}
	gross := notional * m * p.Scale(m)
	fee := gross * WinFee
	return Win{Gross: gross, Fee: fee, Net: gross - fee}
}

// MintRate is PAPER per dollar of loss basis: flat × (decay / (decay + tail))².
// tail is the tokenomics tailProgressUsd high-water mark, which never falls.
func MintRate(flat, decay, tail float64) float64 {
	if tail < 0 {
		tail = 0
	}
	r := decay / (decay + tail)
	return flat * r * r
}

// PairEstimate is one long+short pair closed together after price moved
// Move against the losing leg.
type PairEstimate struct {
	Move         float64 `json:"move"`
	Notional     float64 `json:"notionalPerLeg"`
	Loss         float64 `json:"loss"`
	WinnerNet    float64 `json:"winnerNet"`
	WinnerFee    float64 `json:"winnerFee"`
	NetCost      float64 `json:"netCost"`
	PaperMinted  float64 `json:"paperMinted"`
	CostPerPaper float64 `json:"costPerPaper"`
}

// EstimatePair prices a same-platform pair. queueEmpty selects the 0.98 mint
// basis; with a non-empty queue the full loss mints but winners are queued.
func EstimatePair(p ImpactParams, notional, move, mintRate float64, queueEmpty bool) PairEstimate {
	loss := notional * move
	win := p.WinningClose(notional, move)
	basis := loss
	if queueEmpty {
		basis *= LossMintFactor
	}
	e := PairEstimate{Move: move, Notional: notional, Loss: loss, WinnerNet: win.Net, WinnerFee: win.Fee,
		NetCost: loss - win.Net, PaperMinted: basis * mintRate}
	if e.PaperMinted > 0 {
		e.CostPerPaper = e.NetCost / e.PaperMinted
	}
	return e
}

// MaxSafeMove is the approximate adverse move at which a position at
// leverage busts.
func MaxSafeMove(leverage int) float64 {
	if leverage <= 0 {
		return 0
	}
	return math.Max(0, 1/float64(leverage)-BustBuffer)
}
