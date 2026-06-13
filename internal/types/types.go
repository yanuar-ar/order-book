// Package types defines the engine's plain-old-data value types.
//
// Everything here is fixed-size and pointer-free so values can be written
// directly to the WAL and copied through SPSC rings without allocation.
package types

// Scalar identifiers and fixed-point quantities. All money math is integer
// arithmetic at a configured scale (see pkg/config and package money helpers).
type (
	Price     int64  // fixed-point price, scaled by PriceScale
	Qty       int64  // fixed-point quantity, scaled by QtyScale
	AccountID uint64 // account identifier
	OrderID   uint64 // client/engine order identifier
	MarketID  uint32 // market (shard) identifier
	AssetID   uint32 // asset identifier
	Seq       uint64 // global sequence number, assigned only by the sequencer
)

// Side is the side of an order or the aggressor side of a fill.
type Side uint8

const (
	Buy Side = iota
	Sell
)

// Opposite returns the opposing side.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// OrderType enumerates the supported order types.
type OrderType uint8

const (
	Limit     OrderType = iota // rests remainder at limit price
	Market                     // no price limit; cancels remainder
	Stop                       // trigger -> Market
	StopLimit                  // trigger -> Limit at Price
)

// TIF is the time-in-force policy.
type TIF uint8

const (
	GTC TIF = iota // good-till-cancel
	IOC            // immediate-or-cancel
	FOK            // fill-or-kill
)

// Flags is a bitset of order modifiers.
type Flags uint16

const (
	FlagPostOnly Flags = 1 << iota
	FlagIceberg
)

// Has reports whether all bits in f are set.
func (fl Flags) Has(f Flags) bool { return fl&f == f }

// CmdType enumerates the external command kinds journaled to the WAL.
type CmdType uint8

const (
	CmdNewOrder CmdType = iota
	CmdCancel
	CmdAmend
	CmdDeposit
	CmdWithdraw
)

// Command is a fixed-size, pointer-free external command. It is written
// verbatim to the WAL and transported through rings.
type Command struct {
	Seq        Seq
	TsNanos    int64
	Type       CmdType
	Market     MarketID
	Account    AccountID
	OrderID    OrderID
	Side       Side
	OrdType    OrderType
	Tif        TIF
	Flags      Flags
	Price      Price // limit price, or stop trigger price for Stop/StopLimit
	StopPrice  Price
	Qty        Qty
	DisplayQty Qty     // visible portion for iceberg orders
	Asset      AssetID // for Deposit/Withdraw
	Amount     int64   // for Deposit/Withdraw

	// ClientReqID supports client-side correlation (unused by engine logic).
	ClientReqID uint64
	// ClientTsNanos is a bench-only timestamp for latency correlation. It is
	// never read by engine logic and does not affect determinism.
	ClientTsNanos int64
}

// Fill is the result of one execution between two resting/aggressing orders.
type Fill struct {
	AggressorSeq Seq    // Seq of the aggressor command (deterministic ordering)
	MatchIndex   uint32 // index within the aggressor's match run, resets per aggressor
	Taker        Side   // side that was the aggressor (taker); the other side is the maker
	Market       MarketID
	Price        Price
	Qty          Qty
	BuyOrder     OrderID
	SellOrder    OrderID
	BuyAccount   AccountID
	SellAccount  AccountID
}

// MakerSide returns the resting (maker) side of the fill.
func (f Fill) MakerSide() Side { return f.Taker.Opposite() }

// FundedOrder is the post-reservation envelope the balance authority routes to
// a market shard once funds are reserved. It carries the order fields the
// matcher needs plus the originating command Seq.
type FundedOrder struct {
	Seq        Seq
	Market     MarketID
	Account    AccountID
	OrderID    OrderID
	Side       Side
	OrdType    OrderType
	Tif        TIF
	Flags      Flags
	Price      Price
	StopPrice  Price
	Qty        Qty
	DisplayQty Qty
}

// AckStatus is the outcome reported back to the client.
type AckStatus uint8

const (
	AckAccepted AckStatus = iota
	AckRejected
	AckCanceled
	AckFilled
)

// Ack is the engine's response for a command.
type Ack struct {
	Seq     Seq
	OrderID OrderID
	Account AccountID
	Status  AckStatus
	Reason  RejectReason
	// ClientTsNanos mirrors the command's bench-only timestamp for latency
	// correlation; never read by engine logic.
	ClientTsNanos int64
}

// RejectReason explains a rejection.
type RejectReason uint8

const (
	ReasonNone RejectReason = iota
	ReasonInsufficientFunds
	ReasonPostOnlyCross
	ReasonFOKUnfillable
	ReasonUnknownOrder
	ReasonOverflow
)
