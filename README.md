# alphafox-hedge-trader

AlphaFox 的独立下单交易组件：Hyperliquid 钱包签名下单 + Papertrade 同平台多空挖矿。

Papertrade 是「亏损挖矿」永续：亏损方按亏损额铸造 PAPER，盈利方被收 2% 手续费并受价格冲击折扣。同一账户在 Paper 上同时开等额多空，价格移动后一起平仓：亏损腿铸币，盈利腿收回大部分，净成本 = 冲击折扣 + 2% 手续费。机制与成本测算见 `docs/paper-research.md`。

背景：2026-10-11 会议《Paper交易项目机制讨论与对冲无损挖矿方案策划》；后续决定放弃 HL 对冲腿，改为同平台多空。

## 结构

| 包 | 职责 |
|---|---|
| `pkg/evmsign` | secp256k1 / EIP-712 签名原语与 `Signer` 接口 |
| `pkg/hyperliquid` | 原生 Hyperliquid 客户端（L1 action 签名、下单/撤单/杠杆、info）；`farm` 用它的 l2Book mid 作价格源（Paper 按 HL BBO mid 结算） |
| `pkg/venue` | 统一永续交易所接口（HL CLI 使用） |
| `pkg/paper` | Papertrade relayer 网关：EIP-712 intent 签名（Open / Close / Stake / Unstake / Claim / RegisterSessionKey）、intentId、nonce、relayer 提交与查询（钱包 SSE、成交历史、交易参数、铸造曲线）、成本模型 |
| `pkg/farm` | 多空挖矿循环：开两腿 → 盯价 → 触发后单个 intent 同时平两腿 → 从成交历史结算成本与 PAPER |
| `cmd/hedgectl` | CLI，默认 dry-run（只签名不发送），`-live` 才提交 |

## 使用

```bash
# Paper：只用 session key（能交易/质押，不能提现）；主钱包私钥不进入本仓库
export PAPER_ACCOUNT_ADDRESS=0x...        # Paper 主钱包地址
export PAPER_SESSION_PRIVATE_KEY=0x...    # 已注册的 session key

go run ./cmd/hedgectl paper status                      # 交易参数 + 铸造率（只读）
go run ./cmd/hedgectl paper econ -coin BTC -notional 10000
go run ./cmd/hedgectl paper account                     # 余额 / 仓位 / session key
go run ./cmd/hedgectl farm run -coin BTC -margin 10 -leverage 1000 -close-move 0.0003 -bust-guard 0.0001
                                                         # 只出计划和签名预览，不提交

# 注册 session key（一次性，需主钱包在钱包里签名）
go run ./cmd/hedgectl paper session-new                 # 生成 key，私钥只打印一次
go run ./cmd/hedgectl paper register-data -session 0xKEY > reg.json   # 用主钱包 eth_signTypedData_v4 签名
go run ./cmd/hedgectl paper register -data reg.json -signature 0xSIG  # 校验签名者；加 -live 才提交

# Hyperliquid
export HL_NETWORK=testnet HL_ACCOUNT_ADDRESS=0x... HL_AGENT_PRIVATE_KEY=0x...
go run ./cmd/hedgectl hl quote -coin BTC
```

完整命令见 `go run ./cmd/hedgectl`。

## 小资金约束（约 20U）

Paper 最小保证金 $10、最小开仓名义 $10,000，两腿共 $20 只能用 1000x。1000x 的爆仓距离约 0.05%，所以 `-close-move` 约 0.03%、`-bust-guard` 约 0.01%；按当前铸造率，单位成本约 $0.014/PAPER，是 100x / 0.5% 档的约 2 倍。`farm` 会拒绝 close-move + bust-guard 超过爆仓距离的配置。

## 安全约束

- 默认 dry-run；提交必须显式 `-live`。
- 启动前要求该品种无持仓；`-live` 还检查交易未暂停、session key 已注册且未过期、可用余额 ≥ 2 × 保证金。
- 只有一腿成交时立即平掉该腿；两腿始终在同一个 Close intent 中平仓，未确认则按超时重发。
- Ctrl-C 不会自动平仓：仓位保持并打印，由人工处理。

## 开发

```bash
go test -race ./...
```

所有测试离线：Paper 线格式由链上真实 intent 的 golden 测试锁定，relayer 交互用 httptest，策略用假交易所 + 模拟时钟。
