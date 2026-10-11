package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/shopspring/decimal"

	"github.com/alphafoxai/alphafox-hedge-trader/pkg/evmsign"
	"github.com/alphafoxai/alphafox-hedge-trader/pkg/paper"
)

func paperClient(dryRun bool) *paper.Client {
	url := os.Getenv("PAPER_RELAYER_URL")
	if url == "" {
		url = paper.DefaultRelayerURL
	}
	c := paper.NewClient(url, os.Getenv("PAPER_BUILD"), nil)
	c.DryRun = dryRun
	return c
}

func paperUser() (common.Address, error) {
	raw := os.Getenv("PAPER_ACCOUNT_ADDRESS")
	if !common.IsHexAddress(raw) {
		return common.Address{}, errors.New("PAPER_ACCOUNT_ADDRESS must be the Papertrade master wallet address")
	}
	return common.HexToAddress(raw), nil
}

// paperAccount needs the session key only for commands that sign.
func paperAccount(dryRun bool, ttl time.Duration) (*paper.Account, error) {
	user, err := paperUser()
	if err != nil {
		return nil, err
	}
	key, err := evmsign.NewLocalSigner(os.Getenv("PAPER_SESSION_PRIVATE_KEY"))
	if err != nil {
		return nil, fmt.Errorf("PAPER_SESSION_PRIVATE_KEY: %w", err)
	}
	if key.Address() == user {
		return nil, errors.New("PAPER_SESSION_PRIVATE_KEY is the master wallet key; use a session key")
	}
	return &paper.Account{Client: paperClient(dryRun), Intents: paper.NewIntents(user, key, paper.NewNonces(nil), ttl, nil)}, nil
}

func runPaper(ctx context.Context, cmd string, rest []string) error {
	fs := flag.NewFlagSet("paper "+cmd, flag.ContinueOnError)
	live := fs.Bool("live", false, "post signed intents (default: dry run)")
	coin := fs.String("coin", "BTC", "BTC or ETH")
	notional := fs.Float64("notional", 10_000, "notional per leg, USD")
	moves := fs.String("moves", "0.0003,0.0005,0.001,0.002,0.005,0.008", "adverse moves to price")
	mintRate := fs.Float64("mint-rate", 0, "PAPER per USD of loss basis; 0 reads the live curve")
	limit := fs.Int("limit", 20, "history rows")
	session := fs.String("session", "", "session key address to register")
	days := fs.Int("days", 30, "session key lifetime in days (max 30)")
	dataPath := fs.String("data", "", "typed data JSON from register-data")
	sigHex := fs.String("signature", "", "master wallet signature over -data")
	side := fs.String("side", "", "long or short")
	margin := fs.String("margin", "", "margin per position, USD")
	leverage := fs.Int("leverage", 0, "leverage 1..1000")
	ids := fs.String("ids", "", "comma-separated position ids")
	amount := fs.String("amount", "", "PAPER amount")
	all := fs.Bool("all", false, "stake the whole unstaked PAPER balance")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	switch cmd {
	case "status":
		c := paperClient(true)
		ts, err := c.Trading(ctx)
		if err != nil {
			return err
		}
		s, err := c.Summary(ctx)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"trading": ts, "summary": s, "mintRate": s.MintRate()}, nil)

	case "econ":
		c := paperClient(true)
		impact, ok := paper.LaunchImpact[*coin]
		if !ok {
			return fmt.Errorf("-coin %s: BTC or ETH", *coin)
		}
		rate, queueEmpty := *mintRate, true
		if rate == 0 {
			ts, err := c.Trading(ctx)
			if err != nil {
				return err
			}
			m, err := ts.Market(*coin)
			if err != nil {
				return err
			}
			s, err := c.Summary(ctx)
			if err != nil {
				return err
			}
			impact, rate, queueEmpty = m.Impact, s.MintRate(), s.QueueBalance.IsZero()
		}
		var out []paper.PairEstimate
		for _, raw := range strings.Split(*moves, ",") {
			mv, err := decimal.NewFromString(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("-moves %q: %w", raw, err)
			}
			out = append(out, paper.EstimatePair(impact, *notional, mv.InexactFloat64(), rate, queueEmpty))
		}
		return printJSON(map[string]any{"coin": *coin, "mintRate": rate, "queueEmpty": queueEmpty, "pairs": out}, nil)

	case "account":
		user, err := paperUser()
		if err != nil {
			return err
		}
		return printJSON(paperClient(true).Wallet(ctx, user))

	case "trades":
		user, err := paperUser()
		if err != nil {
			return err
		}
		return printJSON(paperClient(true).Trades(ctx, user, *limit))

	case "session-new":
		key, err := crypto.GenerateKey()
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "New Paper session key. Store the private key as PAPER_SESSION_PRIVATE_KEY (secret store, not a committed file); it is shown once.")
		return printJSON(map[string]string{
			"address":    crypto.PubkeyToAddress(key.PublicKey).Hex(),
			"privateKey": "0x" + hex.EncodeToString(crypto.FromECDSA(key)),
		}, nil)

	case "register-data":
		user, err := paperUser()
		if err != nil {
			return err
		}
		if !common.IsHexAddress(*session) {
			return errors.New("-session must be the session key address")
		}
		if *days < 1 || *days > 30 {
			return errors.New("-days must be 1..30")
		}
		nonce, err := paper.NewNonces(nil).Take(paper.NonceExchange)
		if err != nil {
			return err
		}
		now := time.Now()
		td, err := paper.RegisterSessionKeyTypedData(user, common.HexToAddress(*session),
			now.Add(time.Duration(*days)*24*time.Hour), nonce, now.Add(paper.DefaultIntentTTL))
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Sign this with the master wallet (eth_signTypedData_v4) within one hour, then run `paper register -data <file> -signature <sig>`.")
		return printJSON(td, nil)

	case "register":
		user, err := paperUser()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(*dataPath)
		if err != nil {
			return fmt.Errorf("-data: %w", err)
		}
		var td apitypes.TypedData
		if err := json.Unmarshal(raw, &td); err != nil {
			return fmt.Errorf("-data: %w", err)
		}
		if td.PrimaryType != "RegisterSessionKey" || td.Domain.Name != paper.ExchangeDomain().Name ||
			!strings.EqualFold(td.Domain.VerifyingContract, paper.ExchangeAddress.Hex()) {
			return errors.New("-data is not a Papertrade RegisterSessionKey payload")
		}
		sig, err := parseSignature(*sigHex)
		if err != nil {
			return err
		}
		in, err := paper.AssembleIntent(paper.ActionRegisterSessionKey, td, sig)
		if err != nil {
			return err
		}
		if in.Signer != user {
			return fmt.Errorf("signature is from %s, not PAPER_ACCOUNT_ADDRESS %s", in.Signer.Hex(), user.Hex())
		}
		return printJSON(paperClient(!*live).Submit(ctx, in))

	case "open":
		acct, err := paperAccount(!*live, 0)
		if err != nil {
			return err
		}
		if *side != "long" && *side != "short" {
			return errors.New("-side must be long or short")
		}
		m, err := decimal.NewFromString(*margin)
		if err != nil {
			return fmt.Errorf("-margin: %w", err)
		}
		return printIntent(ctx, acct, func() (paper.Intent, error) {
			in, err := paper.InstrumentBySymbol(*coin)
			if err != nil {
				return paper.Intent{}, err
			}
			return acct.Intents.Open(in, *side == "long", paper.ToRaw(m), *leverage)
		})

	case "close":
		acct, err := paperAccount(!*live, 0)
		if err != nil {
			return err
		}
		return printIntent(ctx, acct, func() (paper.Intent, error) {
			return acct.Intents.Close(strings.Split(*ids, ","))
		})

	case "stake":
		acct, err := paperAccount(!*live, 0)
		if err != nil {
			return err
		}
		var amt *big.Int
		if *all {
			w, err := acct.Client.Wallet(ctx, acct.Intents.User())
			if err != nil {
				return err
			}
			amt, _ = new(big.Int).SetString(w.Paper.Balance, 10)
		} else {
			v, err := decimal.NewFromString(*amount)
			if err != nil {
				return fmt.Errorf("-amount: %w", err)
			}
			amt = paper.ToRaw(v)
		}
		return printIntent(ctx, acct, func() (paper.Intent, error) { return acct.Intents.Stake(amt) })

	case "claim":
		acct, err := paperAccount(!*live, 0)
		if err != nil {
			return err
		}
		return printIntent(ctx, acct, acct.Intents.Claim)
	}
	return errors.New(usage)
}

// printIntent signs, submits (or dry-runs) and prints both the wire intent
// and the relayer's answer.
func printIntent(ctx context.Context, acct *paper.Account, build func() (paper.Intent, error)) error {
	in, err := build()
	if err != nil {
		return err
	}
	res, err := acct.Client.Submit(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"intent": in, "intentId": in.ID.Hex(), "result": res}, nil)
}

func parseSignature(s string) (evmsign.Signature, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(raw) != 65 {
		return evmsign.Signature{}, errors.New("-signature must be a 65-byte hex signature")
	}
	v := int(raw[64])
	if v < 27 {
		v += 27
	}
	return evmsign.Signature{R: "0x" + hex.EncodeToString(raw[:32]), S: "0x" + hex.EncodeToString(raw[32:64]), V: v}, nil
}
