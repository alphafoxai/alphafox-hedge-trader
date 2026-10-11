# Paper 接入（待办）

Paper 没有公开 API，只能通过钱包协议用程序下单。`pkg/paper.Gateway` 是唯一需要实现的接缝：

```go
type Gateway interface {
	Open(ctx, symbol string, side venue.Side, size decimal.Decimal, leverage int) (venue.OrderResult, error)
	Close(ctx, symbol string) (venue.OrderResult, error) // 排队时返回 OrderPending
	Position(ctx, symbol string) (venue.Position, error)
	MarkPrice(ctx, symbol string) (decimal.Decimal, error)
}
```

实现后用 `paper.NewVenue(cfg, gateway)` 替换 `cmd/hedgectl` 中的 `nil`。`pkg/hedge` 无需修改。

## 链上交互调研清单（会议待办：研究 Paper 链上交互流程）

1. 站点、所在链、chainId、RPC。
2. 下单方式：直接调合约（交易签名 + gas），还是签 EIP-712 消息交给 relayer/后端。前者需要给 `evmsign` 增加交易签名；后者复用 `evmsign.Signer.SignTypedData`。
3. 合约地址 / ABI：开仓、平仓、保证金存取、质押。
4. 名义价值与保证金的计算口径（亏损按名义价值还是保证金计 PAPER）、手续费、最小名义（会议提到 1 万 U 名义只需 10 U 保证金）。
5. 价格来源：确认结算价引用 Hyperliquid 的哪个价格（mark / mid / oracle），用于评估两腿价差。
6. 平仓排队：排队时长分布、排队期间价格如何结算、能否取消；决定 `hedge.Config.CloseTimeout`。
7. 仓位/铸造查询：读合约状态还是索引服务；PAPER 铸造曲线与当前单位成本。
8. 质押与分红领取的调用方式（后续自动化）。

## 风险

- 平仓排队导致两腿不同步 → 多空双爆。编排已保证「Paper 未平不动 HL」，但仍需控制杠杆与仓位。
- 两腿开仓存在时间差，价格偏移即真实损益；`MaxSpreadBps` / `MaxBookShare` 只约束 HL 一侧。
- 大户在 HL 低流动性时段操纵价格会同时影响 Paper 结算价，对冲腿不受保护的时段应避开。
