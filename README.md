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
| `pkg/paper/plan.go` | 滚动实现波动率 + 参数推荐（杠杆 / 平仓波动 / 爆仓保护） |
| `pkg/console` | 本地 Web 控制台（`hedgectl web`）：代理 relayer、推荐参数、按品种运行 farm |
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

## Web 控制台

```bash
go run ./cmd/hedgectl web            # http://127.0.0.1:8547，dry-run：只签名预览
go run ./cmd/hedgectl web -live      # 实盘：提交 intent
```

- **凭证只在浏览器**：账户地址和 session key 存在本浏览器 localStorage；页面每次写操作把 key 发给本机服务，服务端不落盘，只在运行中的 farm 内存里持有。可在页面里用 OKX 钱包生成并注册 session key（主钱包只签 `RegisterSessionKey`），也可导入 / 显示 / 清除。
- **为什么要本地服务**：relayer 拒绝跨域请求（`/state/trading` 对外部 Origin 返回 400，`/intents` 不支持 OPTIONS），浏览器无法直连。
- **安全**：只监听 loopback；校验 Host（防 DNS rebinding）；`/api` 需要页面内嵌的进程级随机 token（防 CSRF）。
- **波动率与推荐**：从 Hyperliquid 拉 1 分钟 K 线，按可选窗口（1h / 6h / 24h / 3d）算滚动实现波动率 σ（每 √小时），总资金在 BTC / ETH 间平分，给出：
  - 平仓波动 = σ·√(目标等待小时)，预计每轮耗时 = (波动/σ)²；
  - 爆仓保护 = max(0.05%, 6σ·√10 秒)，且不超过平仓波动的一半；
  - 杠杆 = 可用档位中，爆仓距离（1/杠杆 − 0.05%）≥ 平仓波动 + 保护 的最大档；资金不足以支撑最小名义时提示所需资金。
  - 每 PAPER 成本只取决于平仓波动（波动越大越便宜，0.5% 以上接近 $0.004 下限），所以推荐在可接受的等待时间内取尽量大的波动、尽量低的杠杆。
- **运行**：BTC / ETH 各一张卡片，可「应用推荐」后开始；两个品种可并行（启动时检查可用余额覆盖所有运行中的两腿保证金）。停止可选保留仓位或同时平仓；运行中禁止手动平仓以免冲突。Ctrl-C 退出时停止运行、保留仓位并打印 id。

ETH 的波动比 BTC 高约 1.5–1.8 倍（2026-10-11：BTC σ≈0.11%/√h，ETH≈0.17–0.20%/√h），同样等待时间可用更大的平仓波动；但 ETH 的冲击参数让同等波动下成本约高 10%，两者每 PAPER 成本接近。并行跑两个品种约翻倍产出。

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
