# Papertrade 规则调研（2026-10-11）

来源：官方文档 docs.papertrade.xyz（23 页全文）、exchange.papertrade.xyz 前端代码、HyperEVM 链上只读调用。
链上数据读取于 2026-10-11 约 07:40 UTC（区块 ~48238307）。

## 1. 机制要点（官方文档 + 合约核对）

| 项 | 结论 |
|---|---|
| 链 | HyperEVM 主网，chainId 999；USDC 存在 HyperCore |
| 对手方 | 全部与 LP 对赌，无订单簿、无撮合，HL 上不真实开仓 |
| 价格 | HL 永续盘口 BBO mid `(bid+ask)/2`，经预编译 `0x…080e` 读取（BTC=asset 0，ETH=asset 1）。无偏离保护 |
| 费用 | 无手续费、无点差、无资金费、无 gas（relayer 付）。**只对盈利平仓收费** |
| 盈利平仓 | ① 0.2 bp 死区（出场价按不利 entry/50000 计）② 非对称冲击 haircut ③ 2% 盈利费 |
| 亏损平仓 | 只亏亏损本身；另有 2% 费用从 LP 收益中切出（队列为空时），PAPER 按 `亏损 × 0.98` 铸造；队列非空时免费、按全额铸造 |
| 杠杆/上限 | BTC、ETH，1–1000x；单仓名义 ≤ $10M；仓位不可加保证金/改杠杆，只能整仓平 |
| 强平 | 硬爆仓：价格触及爆仓价（比零权益价早约 5 bp）即**没收全部保证金**；100x 约在 0.95% 触发，1000x 约 0.052% |
| “排队” | 两种：① relayer 意图队列（签名后 `queued`，可取消，有效期 1h）；② LP 不足时**盈利部分**进入 FIFO 兑付队列（本金立即返还）。平仓价格以执行区块的 BBO mid 结算，不存在“平仓延迟成交” |
| 存取款 | 最少 10 USDC；首次存款 1 USDC 激活费；取款需主钱包签名 |
| 下单方式 | 只能通过官方 relayer（`POST /intents`）提交 EIP-712 签名意图，session key 代签（30 天有效）。合约不接受直接调用 |
| PAPER | 不可转账（上线阶段），只能质押/解押，即时无锁定 |

### 非对称冲击公式（合约 `instruments()` 已核对）

```
scale = (1 − baseRate) / (1 + 1/(move·rateMultiplier) + referenceNotional/(10⁶·move·positionMultiplier))
```

链上参数：baseRate 10%，rateMultiplier 15000，referenceNotional $100k，positionMultiplier BTC 814.598 / ETH 483.979。
盈利方最终到手（含死区 + 2% 费）占原始盈利：BTC 0.1% 波动 72.4%，1% 波动 86.4%；ETH 0.1% 67.6%，1% 85.7%。
**haircut 只看价格波动幅度，与仓位大小无关。**

### PAPER 铸造曲线（PaperTokenomics 已核对）

`rate = 100 · (S / (S + H))²`，S = $120M，H = `tailProgressUsd`（LP 超过 $2M 后累计收益的历史最高水位，**只增不减**）。

| 链上值 | 数值 |
|---|---|
| trackedLpUsd | $5,003,097 |
| tailProgressUsd (H) | $77,876,520 |
| **当前铸造率** | **36.78 PAPER / $1 亏损基数** |
| PAPER 总供应 | 5,379.7M；已质押 5,312.9M（98.8%） |
| 兑付队列 | 0（LP 可用 $4,999,949，健康） |
| 24h 成交名义 | BTC ≈ $300.9B，ETH ≈ $331.7B |

铸造率只取决于累计高水位，不取决于当前 LP：H 到 $100M → 29.8，$150M → 19.8，$200M → 14.1。越晚越少，且不可逆。

### 分红来源

1. 2% 费用的一半（队列为空且 LP 可覆盖时）。
2. LP 超过 $5M 上限的部分**全部**分给质押者。当前 LP 已在上限，即：所有交易者的净亏损（扣除付给赢家的部分）≈ 全部流向质押者。

## 2. 对冲成本（按链上参数精确计算）

### 同平台多空对冲（Paper 多 + Paper 空，同名义）

每边 100U 保证金 × 100x = 10,000U 名义，BTC，队列为空，铸造率 36.78：

| 波动 | 亏损边 | 盈利边到手 | 净损耗 | 获得 PAPER | 成本/PAPER |
|---|---|---|---|---|---|
| 0.05% | 5.00 | 3.04 | 1.96 | 180 | $0.0109 |
| **0.10%** | **10.00** | **7.24** | **2.76** | **360** | **$0.0077** |
| 0.20% | 20.00 | 15.94 | 4.06 | 721 | $0.0056 |
| 0.50% | 50.00 | 42.31 | 7.69 | 1,802 | $0.0043 |
| 0.80% | 80.00 | 68.75 | 11.25 | 2,883 | $0.0039 |

- 波动越大越划算：成本占亏损比例从 0.1% 时 27.6% 降到 0.8% 时 14.1%，下限约 12%。
- 必须在亏损边爆仓前主动平：硬爆仓没收全部保证金，多亏的部分没有对冲回收。100x 下平仓阈值应 < 0.9%。

### 跨平台（Paper 一边 + HL 反向）

HL 基础费率按 taker 0.045% / maker 0.015% 每边估算，未含资金费：

- 方向不可预知，**一半概率 Paper 那边是赢家**：此时 Paper 被 haircut、HL 真实亏损、不铸造 PAPER。
- 对称平仓时期望成本/PAPER = 同平台成本 + 2×HL 手续费 / 铸造基数，**永远比同平台贵**（0.1% 波动、taker：$0.058 vs $0.0077）。
- 只有“只在 Paper 腿亏损时才平仓、Paper 盈利时继续持有”才更便宜（0.5% 波动 + maker：$0.0017/PAPER），代价是持仓时间不可控、要付 HL 资金费、占用两边资金。

## 3. 对会议方案的修正

1. “Paper 开单亏损 + HL 对冲 = 只损失手续费”只在 Paper 腿恰好亏损时成立。实际一半时间是反过来的，所以 `pkg/hedge` 的平仓逻辑必须改成有条件的：只在 Paper 腿亏损达到阈值时平仓。
2. “平仓排队”不是成交延迟，是 relayer 意图排队 + 盈利兑付排队。平仓价格在执行区块锁定；HL 腿等待的是 relayer 落链（秒级），不是 30s+ 的结算排队。
3. 同平台多空对冲（不用 HL）在多数情况下就是最低成本方案，并且不需要 HL 那一腿。

## 4. 经济学提醒

- 对冲刷子付出的净损耗 = LP 收入 → 当 LP 在 $5M 上限时全部分给质押者。纯对冲刷单整体上接近零和：长期分红/PAPER ≈ 刷单成本/PAPER。真正的利润来自方向性交易者的净亏损和“比别人早”（铸造率持续下降）。
- PAPER 目前不可转让，价值只来自分红；合约可升级（owner 多签 + timelock），规则可能变化。
- BBO 可被操纵、无偏离熔断；LP 若跌破需求，盈利会进入兑付队列（延迟，极端情况长期拿不到）。

## 5. 接入信息（实现 `paper.Gateway` 用）

| 项 | 值 |
|---|---|
| RPC | `https://rpc.hyperliquid.xyz/evm` |
| Exchange（代理） | `0x6cd5661646289fb6e65ea5c032310fded797d0a2`（实现 `0x0f3f…49ee`） |
| PAPER | `0xe40f17915daa230324030003a197cdaef2261c0e` |
| PaperStaking | `0xaad6c7b0cc3014fc80ffedae0ed7ce5967b0f016` |
| PaperTokenomics | `0x75174549dbbd44497d31a47e2e2d7406408d63d9` |
| DepositProxyFactory | `0x1629b7d46bef6e13ff754174fc9f32dea309b38f` |
| Relayer | `https://exchange.papertrade.xyz/intents`（POST），状态查询 `/query/...` |
| instrumentId | BTC 0，ETH 1 |

EIP-712 类型（前端代码）：

- `OpenPosition(address user, uint32 instrumentId, bool isLong, uint128 size, uint32 leverage, bool useDebt, uint256 nonce, uint64 deadline)`：`size` 是保证金，名义 = size × leverage。
- `Close(address user, uint256[] positionIds, uint256 nonce, uint64 deadline)`：最多 25 个仓位。
- `RegisterSessionKey(address user, address key, uint64 expiry, address feeAddress, uint256 nonce, uint64 deadline)`。
- deadline = 当前秒 + 3600。

域名 `{name: "String", version: "1"}` 已用链上真实 Open/Close intent 核对（恢复出的签名者与 intentId 一致），见 `pkg/paper/intent_test.go`。

可读的链上视图：Exchange `treasury / sideBucket / totalQueued / lpBankrollAvailable / instruments(uint32) / positions(uint256) / balances(address)`，Tokenomics `trackedLpUsd / tailProgressUsd / flatRate / lpFlatThresholdUsd / tailDecayScaleUsd`。

## 6. 同平台多空（2026-10-11 补充）

放弃 HL 对冲腿：同一账户开等额多空，任一腿逆向移动达到阈值时用一个 Close intent 同时平两腿。

- 结算：亏损腿铸造 PAPER（队列为空时铸造基数 = 亏损 × 0.98），盈利腿实收 adjustedPnl − 2%。真实同块平仓（tx `0x1f91865e…`，BTC 200x）与模型误差 < 0.25%。
- 最小保证金 $10、最小名义 $10k：20U 只能 1000x，爆仓距离 ≈ 1/lev − 0.0005 ≈ 0.05%。
- 成本（BTC，每腿 $10k 名义，铸造率 ≈ 36.7）：移动 0.1% ≈ $0.0077/PAPER，0.5% ≈ $0.0043；1000x 只能做 ≈0.03% 移动，≈ $0.014/PAPER。资金越多越能用低杠杆、大移动，单位成本越低。
