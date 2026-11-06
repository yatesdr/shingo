package plantspec

import "strings"

// One plant file, one Core, and the Edges that run its processes.
//
// Core's half of a plant (storage, stations, bins, payloads, loaders, kanbans)
// has no Edge: every Edge on a Core shares it. The Edge's half (processes and
// what hangs off them) belongs to one station. A process names its station in
// Process.Edge; blank is the plant's own namespace.line_id. seeddev seeds Core
// from the whole file and an Edge from ForEdge(its station).

// Station is the plant's own Edge station uid, namespace.line_id: the Edge a
// process with no edge of its own runs on.
func (p *Plant) Station() string { return p.Namespace + "." + p.LineID }

// EdgeOf is the station a process runs on; blank for a process the plant does
// not declare.
func (p *Plant) EdgeOf(process string) string {
	for _, pr := range p.Processes {
		if pr.Name == process {
			if pr.Edge != "" {
				return pr.Edge
			}
			return p.Station()
		}
	}
	return ""
}

// Edges is every station the plant's processes run on, the plant's own first
// and the rest in the order the processes name them.
func (p *Plant) Edges() []string {
	out := []string{p.Station()}
	seen := map[string]bool{p.Station(): true}
	for _, pr := range p.Processes {
		if pr.Edge != "" && !seen[pr.Edge] {
			seen[pr.Edge] = true
			out = append(out, pr.Edge)
		}
	}
	return out
}

// ForEdge is the plant as one Edge seeds it: Core's half whole, and of the
// Edge's half only what belongs to the processes on station. The namespace and
// line are the station's. The plant itself is not changed.
func (p *Plant) ForEdge(station string) *Plant {
	out := *p
	out.Namespace, out.LineID = splitStation(station)
	procs := map[string]bool{}
	out.Processes = nil
	for _, pr := range p.Processes {
		if p.EdgeOf(pr.Name) == station {
			procs[pr.Name] = true
			out.Processes = append(out.Processes, pr)
		}
	}
	styles := map[string]bool{}
	out.Styles = nil
	for _, s := range p.Styles {
		if procs[s.Process] {
			styles[s.Name] = true
			out.Styles = append(out.Styles, s)
		}
	}
	built := map[string]bool{}
	out.Claims = nil
	for _, c := range p.Claims {
		if styles[c.Style] {
			out.Claims = append(out.Claims, c)
			if c.OperatorStation != "" {
				built[c.OperatorStation] = true
			}
		}
	}
	out.ReportingPoints = nil
	for _, rp := range p.ReportingPoints {
		// A point with no style names no process: it stays with the plant's own Edge.
		if styles[rp.Style] || (rp.Style == "" && station == p.Station()) {
			out.ReportingPoints = append(out.ReportingPoints, rp)
		}
	}
	out.StationCounters = nil
	for _, sc := range p.StationCounters {
		if procs[sc.Process] {
			out.StationCounters = append(out.StationCounters, sc)
		}
	}
	out.CellConfigs = nil
	for _, cc := range p.CellConfigs {
		if procs[cc.Process] {
			out.CellConfigs = append(out.CellConfigs, cc)
			built[cc.Station] = true
		}
	}
	out.OperatorStations = nil
	for _, s := range p.OperatorStations {
		if built[s] {
			out.OperatorStations = append(out.OperatorStations, s)
		}
	}
	return &out
}

// splitStation is a station uid's namespace and line: the part before the
// first dot and the rest.
func splitStation(station string) (namespace, line string) {
	if i := strings.Index(station, "."); i >= 0 {
		return station[:i], station[i+1:]
	}
	return station, ""
}
