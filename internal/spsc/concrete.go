package spsc

import "github.com/yanuar-ar/order-book/internal/types"

// Concrete ring aliases for the engine's hottest links. They are type aliases
// over the generic Ring so call sites read clearly without re-specifying T.
type (
	RingCommand = Ring[types.Command]
	RingFunded  = Ring[types.FundedOrder]
	RingFill    = Ring[types.Fill]
	RingAck     = Ring[types.Ack]
)

// NewCommand returns a command ring of the given power-of-two capacity.
func NewCommand(capacity uint64) *RingCommand { return New[types.Command](capacity) }

// NewFunded returns a funded-order ring of the given power-of-two capacity.
func NewFunded(capacity uint64) *RingFunded { return New[types.FundedOrder](capacity) }

// NewFill returns a fill ring of the given power-of-two capacity.
func NewFill(capacity uint64) *RingFill { return New[types.Fill](capacity) }

// NewAck returns an ack ring of the given power-of-two capacity.
func NewAck(capacity uint64) *RingAck { return New[types.Ack](capacity) }
