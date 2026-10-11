# Paper 接入状态

> 机制、经济学与调研过程见 `paper-research.md`。

## 已实现并离线验证

| 项 | 结论 | 锁定方式 |
|---|---|---|
| 提交 | `POST {relayer}/intents`，body `{action, signed}`；头 `X-Papertrade-Build` | httptest |
| Exchange 域 | `{name:"String", version:"1", chainId:999, verifyingContract: Exchange}` | 链上 Open/Close intent golden |
| Staking 域 | `{name:"PaperStaking", version:"1", chainId:999, verifyingContract: PaperStaking}`，session key 签 | 单测 |
| intentId | `keccak256(keccak256("papertrade:intent-id:v2") ‖ digest ‖ signer)` | golden |
| signed | 各字段线格式（uint 十进制字符串、地址小写）+ `v` 字符串 "27/28"、`r/s` 小写 hex | golden |
| nonce | 客户端随机 24bit × 256 起，每个 intent +1，Exchange/Staking 各一条序列 | 单测 |
| RegisterSessionKey | 主钱包签名，`feeAddress` 为零地址 | 单测 |
| 仓位 | `/state/user/live?wallet=` SSE 的 `wallet` 事件 `openPositions` 元组 | 真实样本 |
| 结算 | `/query/accounts/{addr}/trades`；盈利方实收 = adjustedPnl − fee | 真实样本 |
| 参数 | `/state/trading`（暂停、最小保证金 $10、最小名义 $10k、市场参数）；`/query/protocol/summary`（铸造曲线） | 真实样本 |

## 上线前仍需真实验证（需授权）

1. 主钱包签 `RegisterSessionKey` 并提交，确认 relayer 接受。
2. 充值约 20U，跑 1 个 `farm run -live` 周期，核对实际成本/PAPER 与 `paper econ` 估算。
3. 未知项：relayer 是否接受非默认 deadline（`-intent-ttl`）；旧 build 头是否被拒；爆仓在成交历史中的 `kind` 名称。
