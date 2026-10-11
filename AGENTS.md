# AGENTS.md

AlphaFox workspace rules apply (`~/Desktop/Projects/alphafox/AGENTS.md`); load them explicitly, this clone lives in `alphafox-git/` and does not inherit them. Reply in Chinese by default.

- Go module; run `gofmt` and `go test -race ./...` before committing.
- `pkg/hyperliquid/action.go` field order is part of the signature. Any wire change must keep `TestL1ActionMatchesOfficialSDK` green; regenerate vectors per `docs/golden-vectors.md`, never hand-edit them.
- Strategy code depends on `pkg/venue`, never on a concrete venue package.
- Writes default to dry run. Never commit private keys or `.env` files; only agent-wallet keys belong in runtime config.
- `pkg/hedge` sequencing (hedge→mint on open, mint→confirm flat→hedge on close) is a safety invariant; change it only with a test showing the new order.
- Live mainnet orders, funding, and deployment need explicit authorization.
