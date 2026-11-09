// What Core and Edge must present or answer identically: UI tokens, status classes and JS utilities, cross-surface answers and cross-module test fixtures; also the repo's Go tooling under scripts/.
module shingo/shared

go 1.25.0

require shingo/protocol v0.0.0

require github.com/google/uuid v1.6.0 // indirect

replace shingo/protocol => ../protocol
