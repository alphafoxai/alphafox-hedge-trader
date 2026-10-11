# alphafox-hedge-trader

AlphaFox 的独立下单交易组件：Hyperliquid 钱包签名下单 + Paper 交易接口（预留）+ 对冲编排。
用于 Paper 「亏损挖矿」的对冲无损挖矿方案：Paper 开单主动亏损铸造 PAPER，Hyperliquid 开反向同规模仓位对冲，整体只损失手续费。

背景：2026-10-11 会议《Paper交易项目机制讨论与对冲无损挖矿方案策划》。

## 结构

| 包 | 职责 |
|---|---|
| `pkg/evmsign` | secp256k1 / EIP-712 签名原语与 `Signer` 接口（移植自 engine `pkg/walletsigner/sign.go`，去掉托管/KMS/存储层） |
| `pkg/hyperliquid` | 原生 Hyperliquid 客户端：通用 L1 action 签名（msgpack + phantom agent）、order / cancel / updateLeverage、info（meta、l2Book、clearinghouseState、openOrders）、价格/数量精度、单调 nonce、dry-run |
| `pkg/venue` | 统一永续交易所接口 `Venue`，对冲逻辑只依赖它 |
| `pkg/paper` | **Paper 预留接口**：`paper.Venue` + 待实现的 `Gateway`；未接入时所有调用返回 `venue.ErrNotImplemented`。附 `SimulatedGateway`（含平仓排队）用于演练 |
| `pkg/hedge` | 对冲编排：开仓先 HL 后 Paper（按 HL 实际成交量下 Paper），Paper 失败则回滚 HL；平仓先 Paper、确认已平后再平 HL，超时则保留 HL 对冲仓 |
| `cmd/hedgectl` | CLI，默认 dry-run（只签名不发送），`-live` 才真实下单 |

与 engine 的关系：engine 的 HL 下单走 CCXT fork（`pkg/exchanges`，约 2.3 万行，深度耦合），本仓库不依赖 CCXT，自己实现 L1 签名，签名结果与官方 Python SDK 逐字节一致（见 `docs/golden-vectors.md`）。engine 暂未改为依赖本仓库。

## 使用

```bash
export HL_NETWORK=testnet                 # mainnet | testnet，必填，无默认值
export HL_ACCOUNT_ADDRESS=0x...           # 主钱包地址（查仓位用）
export HL_AGENT_PRIVATE_KEY=0x...         # Agent(API) 钱包私钥，切勿使用主钱包私钥
# 可选：HL_BUILDER_ADDRESS / HL_BUILDER_FEE（十分之一 bp，50 = 0.05%）

go run ./cmd/hedgectl hl quote -coin BTC
go run ./cmd/hedgectl hl position -coin BTC
go run ./cmd/hedgectl hl order -coin ETH -side buy -size 0.01            # 市价（IOC + 滑点），dry-run
go run ./cmd/hedgectl hl order -coin ETH -side buy -size 0.01 -price 2000 -tif Alo -live
go run ./cmd/hedgectl hl cancel -coin ETH -oid 123 -live
go run ./cmd/hedgectl hl leverage -coin BTC -leverage 10 -live
go run ./cmd/hedgectl hedge open -coin BTC -mint-side buy -size 0.001     # Paper 未接入：下单前即拒绝
go run ./cmd/hedgectl hedge close -coin BTC
```

## 安全约束

- 默认 dry-run；真实下单必须显式 `-live`。
- 只用 Agent 钱包私钥；主钱包私钥、提现能力不进入本仓库。
- `hedge open` 先探测 Paper 是否可用，不可用时不会发出任何 HL 订单。
- Paper 平仓排队期间不平 HL；超时返回错误并保留 HL 对冲仓，由人工处理。
- 先小资金在 testnet / 小仓位验证，避免多空双爆。

## 开发

```bash
go test -race ./...
```

Paper 接入待办见 `docs/paper-integration.md`。
