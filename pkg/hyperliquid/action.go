package hyperliquid

// Wire structs for L1 actions. Field order is part of the signature: the
// action hash is msgpack of these structs in declaration order, and must match
// the official SDK's dict insertion order byte for byte. Do not reorder fields.

type OrderAction struct {
	Type     string       `msgpack:"type" json:"type"`
	Orders   []OrderWire  `msgpack:"orders" json:"orders"`
	Grouping string       `msgpack:"grouping" json:"grouping"`
	Builder  *BuilderWire `msgpack:"builder,omitempty" json:"builder,omitempty"`
}

type OrderWire struct {
	Asset      int           `msgpack:"a" json:"a"`
	IsBuy      bool          `msgpack:"b" json:"b"`
	Price      string        `msgpack:"p" json:"p"`
	Size       string        `msgpack:"s" json:"s"`
	ReduceOnly bool          `msgpack:"r" json:"r"`
	Type       OrderTypeWire `msgpack:"t" json:"t"`
	Cloid      string        `msgpack:"c,omitempty" json:"c,omitempty"`
}

type OrderTypeWire struct {
	Limit *LimitWire `msgpack:"limit,omitempty" json:"limit,omitempty"`
}

type LimitWire struct {
	TIF string `msgpack:"tif" json:"tif"`
}

// BuilderWire attaches a builder fee; F is in tenths of a basis point
// (50 = 0.05%). B must be lowercase.
type BuilderWire struct {
	B string `msgpack:"b" json:"b"`
	F int    `msgpack:"f" json:"f"`
}

type CancelAction struct {
	Type    string       `msgpack:"type" json:"type"`
	Cancels []CancelWire `msgpack:"cancels" json:"cancels"`
}

type CancelWire struct {
	Asset int    `msgpack:"a" json:"a"`
	Oid   uint64 `msgpack:"o" json:"o"`
}

type UpdateLeverageAction struct {
	Type     string `msgpack:"type" json:"type"`
	Asset    int    `msgpack:"asset" json:"asset"`
	IsCross  bool   `msgpack:"isCross" json:"isCross"`
	Leverage int    `msgpack:"leverage" json:"leverage"`
}

func NewOrderAction(orders []OrderWire, builder *BuilderWire) OrderAction {
	return OrderAction{Type: "order", Orders: orders, Grouping: "na", Builder: builder}
}

func NewCancelAction(cancels []CancelWire) CancelAction {
	return CancelAction{Type: "cancel", Cancels: cancels}
}

func NewUpdateLeverageAction(asset int, cross bool, leverage int) UpdateLeverageAction {
	return UpdateLeverageAction{Type: "updateLeverage", Asset: asset, IsCross: cross, Leverage: leverage}
}
