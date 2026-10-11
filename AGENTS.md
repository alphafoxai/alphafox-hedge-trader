# AGENTS.md

AlphaFox workspace rules apply (`~/Desktop/Projects/alphafox/AGENTS.md`); load them explicitly, this clone lives in `alphafox-git/` and does not inherit them. Reply in Chinese by default.

- Go module; run `gofmt` and `go test -race ./...` before committing.
- `pkg/hyperliquid/action.go` field order is part of the signature. Any wire change must keep `TestL1ActionMatchesOfficialSDK` green; regenerate vectors per `docs/golden-vectors.md`, never hand-edit them.
- `pkg/paper` intent types, domains and intent-id derivation are pinned by golden intents copied from chain (`TestGoldenOnChain*`). Never edit those vectors to make a change pass; a wire change must reproduce a real on-chain intent.
- Strategy code depends on interfaces (`pkg/venue`, `farm.Exchange`), never on a concrete venue package.
- Writes default to dry run. Never commit private keys or `.env` files. Runtime config holds only Hyperliquid agent keys and Paper session keys; the Paper master wallet only signs `RegisterSessionKey` out of band and its key never enters this repo.
- `pkg/farm` safety invariants, each covered by a test: refuse to start when the symbol is not flat; a one-legged open is closed immediately; both legs close in one intent; cancellation never leaves an unlogged position; the bust guard closes before liquidation. Change one only with a test showing the new behaviour.
- Live mainnet intents, session-key registration, funding, and deployment need explicit authorization.
