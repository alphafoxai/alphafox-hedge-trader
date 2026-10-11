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

## 实盘验证（2026-10-11）

1. `paper register-web -live`：OKX 钱包签 RegisterSessionKey，relayer 接受，session key 随即出现在钱包 SSE 中。
2. `farm run -live`（BTC 1000x，每腿 $10）：两腿 3 秒内同价 83004.5 开出，爆仓价距入场 0.0525%。
3. 手动 `paper close -ids 136880,136881 -live`：一个 Close intent 同时平两腿，都以 83004.5 平仓，盈亏/手续费/铸造均为 0。
4. 新发现：trades 路由只接受 `limit=75`，其他值 HTTP 400；客户端固定请求 75 再本地截断。

5. 浏览器直连 relayer 被 CORS 拒绝（`/state/trading` 对外部 Origin 返回 400，`/intents` 的 OPTIONS 返回 405；仅 SSE 有 `ACAO: *`），因此 Web 控制台走本机代理。

仍未验证：真实价格移动后的成本/PAPER 与 `paper econ` 的偏差；relayer 是否接受非默认 deadline；旧 build 头是否被拒；爆仓在成交历史中的 `kind`。
