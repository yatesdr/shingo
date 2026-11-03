//go:build docker

package scenarios

import coreorders "shingocore/store/orders"

// coreCart is the one cart a Core order carries (its rows, Order.OnlyBin), nil
// when it carries none or several: these scenarios ask it of single-cart
// orders.
func coreCart(o *coreorders.Order) *int64 {
	b, err := o.OnlyBin()
	if err != nil {
		return nil
	}
	return b
}
