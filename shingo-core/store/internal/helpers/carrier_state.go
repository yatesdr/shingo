package helpers

import "shingo/protocol"

// CarrierStateSQL is protocol.CarrierStateOf in SQL: a CASE over the bin
// aliased b and its type aliased bt (bin_types.bare, bin_types.totes_per_layer)
// that renders the same values, so a statement can select a cart's state or
// test it beside the rows it already reads. Two aliases because the state
// reads both tables. Here with the other shared fragments because more than
// one store aggregate reads it (the empties' finders and counts, and the
// keeper's inbound rows), and store sub-packages may not import each other.
// Pinned equal to the Go by store/bins TestCarrierState_GoSQLAgree.
func CarrierStateSQL(b, bt string) string {
	return `(CASE
		WHEN ` + bt + `.bare THEN '` + string(protocol.CarrierBare) + `'
		WHEN COALESCE(` + b + `.payload_code, '') <> '' THEN '` + string(protocol.CarrierLoaded) + `'
		WHEN ` + bt + `.totes_per_layer IS NULL THEN '` + string(protocol.CarrierEmpty) + `'
		WHEN ` + b + `.totes_on_cart IS NULL THEN '` + string(protocol.CarrierUnverified) + `'
		WHEN ` + b + `.totes_on_cart = 0 THEN '` + string(protocol.CarrierTopsAndBottoms) + `'
		WHEN ` + b + `.totes_on_cart % ` + bt + `.totes_per_layer <> 0 THEN '` + string(protocol.CarrierLeftover) + `'
		ELSE '` + string(protocol.CarrierEmpty) + `' END)`
}
