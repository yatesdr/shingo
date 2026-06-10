package messaging

import (
	"sort"

	"shingo/protocol"
	"shingoedge/store/counters"
)

// BuildCellCatalog groups an edge's reporting points by PLCName into the cell
// catalog the edge sends on registration (Q-034). Each distinct PLC becomes one
// cell; the reporting points on that PLC become its process bindings. The
// catalog describes the plant's wiring, so disabled points are included too —
// liveness is a separate signal. Order is deterministic (cell label, then
// process id) so re-registration with an unchanged point set produces a
// byte-identical payload (no spurious upserts on the core side).
func BuildCellCatalog(points []counters.ReportingPoint) []protocol.CellCatalogEntry {
	byPLC := map[string][]protocol.CellProcessBinding{}
	for _, p := range points {
		if p.PLCName == "" {
			continue
		}
		byPLC[p.PLCName] = append(byPLC[p.PLCName], protocol.CellProcessBinding{
			ProcessID: p.ProcessID,
			StyleID:   p.StyleID,
			PLCName:   p.PLCName,
			TagName:   p.TagName,
		})
	}
	out := make([]protocol.CellCatalogEntry, 0, len(byPLC))
	for label, procs := range byPLC {
		sort.Slice(procs, func(i, j int) bool { return procs[i].ProcessID < procs[j].ProcessID })
		out = append(out, protocol.CellCatalogEntry{CellLabel: label, Processes: procs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CellLabel < out[j].CellLabel })
	return out
}
