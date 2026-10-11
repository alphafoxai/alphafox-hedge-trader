package paper

import (
	"context"
	"math/big"

	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/farm"
)

// Account binds one wallet's intents to the relayer and implements
// farm.Exchange.
type Account struct {
	Client  *Client
	Intents *Intents
}

var _ farm.Exchange = (*Account)(nil)

func (a *Account) submit(ctx context.Context, in Intent, err error) (string, error) {
	if err != nil {
		return "", err
	}
	res, err := a.Client.Submit(ctx, in)
	if err != nil {
		return "", err
	}
	return res.IntentID, nil
}

func (a *Account) Open(ctx context.Context, symbol string, isLong bool, margin decimal.Decimal, leverage int) (string, error) {
	in, err := InstrumentBySymbol(symbol)
	if err != nil {
		return "", err
	}
	intent, err := a.Intents.Open(in, isLong, ToRaw(margin), leverage)
	return a.submit(ctx, intent, err)
}

func (a *Account) Close(ctx context.Context, positionIDs []string) (string, error) {
	intent, err := a.Intents.Close(positionIDs)
	return a.submit(ctx, intent, err)
}

func (a *Account) Stake(ctx context.Context, amount *big.Int) (string, error) {
	intent, err := a.Intents.Stake(amount)
	return a.submit(ctx, intent, err)
}

func (a *Account) Positions(ctx context.Context) ([]farm.Position, error) {
	w, err := a.Client.Wallet(ctx, a.Intents.User())
	if err != nil {
		return nil, err
	}
	out := make([]farm.Position, 0, len(w.OpenPositions))
	for _, p := range w.OpenPositions {
		fp, err := toFarmPosition(p)
		if err != nil {
			return nil, err
		}
		out = append(out, fp)
	}
	return out, nil
}

func toFarmPosition(p Position) (farm.Position, error) {
	in, err := InstrumentByID(p.InstrumentID)
	if err != nil {
		return farm.Position{}, err
	}
	entry, err := in.Price(p.EntryPriceRaw)
	if err != nil {
		return farm.Position{}, err
	}
	bust, err := in.Price(p.BustPriceRaw)
	if err != nil {
		return farm.Position{}, err
	}
	margin, err := FromRaw(p.MarginRaw)
	if err != nil {
		return farm.Position{}, err
	}
	return farm.Position{ID: p.ID, IntentID: p.IntentID, Symbol: in.Symbol, IsLong: p.IsLong,
		Entry: entry, Bust: bust, Margin: margin, Leverage: p.Leverage}, nil
}

// Settled finds the closes for positionIDs in recent history.
func (a *Account) Settled(ctx context.Context, positionIDs []string) ([]farm.Settled, error) {
	trades, err := a.Client.Trades(ctx, a.Intents.User(), 0)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range positionIDs {
		want[id] = true
	}
	var out []farm.Settled
	for _, t := range trades {
		if !want[t.PositionID] {
			continue
		}
		s, err := toSettled(t)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
		delete(want, t.PositionID)
	}
	return out, nil
}

func toSettled(t Trade) (farm.Settled, error) {
	in, err := InstrumentByID(t.InstrumentID)
	if err != nil {
		return farm.Settled{}, err
	}
	s := farm.Settled{PositionID: t.PositionID, IsLong: t.IsLong, Kind: t.Kind}
	if s.Exit, err = in.Price(t.ExitPriceRaw); err != nil {
		return farm.Settled{}, err
	}
	pnl, err := FromRaw(t.AdjustedPnlRaw)
	if err != nil {
		return farm.Settled{}, err
	}
	if s.Fee, err = FromRaw(t.UserPaidFeeRaw); err != nil {
		return farm.Settled{}, err
	}
	if s.PaperMinted, err = FromRaw(t.PaperMintedRaw); err != nil {
		return farm.Settled{}, err
	}
	// adjustedPnl is the winner's gain after impact; the win fee comes off
	// it. On chain the fee is exactly 2% of adjustedPnl.
	s.PnL = pnl.Sub(s.Fee)
	return s, nil
}
