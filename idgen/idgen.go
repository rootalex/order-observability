// Package idgen generates order identifiers.
package idgen

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/rootalex/order-observability/domain"
)

type Random struct{}

func (Random) NewOrderID() domain.OrderID {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return domain.OrderID("ord_" + hex.EncodeToString(b))
}
