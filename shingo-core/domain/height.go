package domain

import "shingo/protocol"

// Height is the height an empty cart is wanted at, in layers of totes: the
// value and its three forms (protocol.Layers, protocol.TopsAndBottomsOnly, the
// zero value for none) and the rule that reads it (Admits) live in protocol,
// because the want rides the wire. Core ranks the carts a height admits by the
// ladder (bins preferredEmptyOrder, and its Go twin for the loader pool,
// binsource rank): the exact height first, then the shortest taller, then the
// tallest shorter; at 0, tops & bottoms first, else the shortest stack.
//
// It replaces the *int that every empty finder, the finder's want and the
// set-down read carried, where nil was none.
type Height = protocol.Height
