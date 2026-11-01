package plantspec

import (
	"strings"
	"testing"
)

// kanban_press_test.go — plants/kanban-press.yaml, the Kanban certification rig.
// SHAPE, not tuning: what each run needs the file to have. Its arithmetic is
// pinned beside it in cmd/seeddev (TestKanbanPressFixture_*).

func loadKanbanPress(t *testing.T) *Plant {
	t.Helper()
	p, err := Load("../../plants/kanban-press.yaml")
	if err != nil {
		t.Fatalf("load plants/kanban-press.yaml: %v", err)
	}
	return p
}

func TestKanbanPressPlant_ValidAndCleanAtBirth(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress(t)
	if err := p.Validate(); err != nil {
		t.Fatalf("plants/kanban-press.yaml invalid: %v", err)
	}
	if c := p.CensusAtBirth(); !c.Clean() {
		t.Errorf("census: %v", c.Findings())
	}
	if c := p.CarrierPairsAtBirth(); !c.Clean() {
		t.Errorf("carrier pairs: %v", c.Findings())
	}
	if p.BaselineFrozenAt != "" {
		t.Errorf("the rig is frozen as a baseline by %q; its census must refuse, not report", p.BaselineFrozenAt)
	}
}

// The press: two-off press-index on four positions, the same four under every
// style; styles on three types of one floor and one of another; a two-part style;
// a part with no carrier.
func TestKanbanPressPlant_ThePress(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress(t)

	carrierOf := map[string]string{}
	noCarrier := []string{}
	for _, pl := range p.Payloads {
		carrierOf[pl.Code] = pl.BinType
		if pl.NoBinType {
			noCarrier = append(noCarrier, pl.Code)
		}
	}
	if len(noCarrier) != 1 {
		t.Errorf("parts with no carrier = %v, want the one", noCarrier)
	}

	stylesOfPress := map[string]bool{}
	for _, s := range p.Styles {
		if s.Process == "KP-PRESS" {
			stylesOfPress[s.Name] = true
		}
	}
	positions := map[string]bool{}
	partsByStyle := map[string]map[string]bool{}
	typesByStyle := map[string]map[string]bool{}
	for _, c := range p.Claims {
		if !stylesOfPress[c.Style] {
			continue
		}
		if c.SwapMode != "two_robot_press_index" || c.Role != "produce" {
			t.Errorf("claim %s/%s: %s %s, want a produce press-index", c.CoreNode, c.Style, c.Role, c.SwapMode)
		}
		positions[c.CoreNode], positions[c.PairedCoreNode] = true, true
		if partsByStyle[c.Style] == nil {
			partsByStyle[c.Style], typesByStyle[c.Style] = map[string]bool{}, map[string]bool{}
		}
		partsByStyle[c.Style][c.Payload] = true
		if bt := carrierOf[c.Payload]; bt != "" {
			typesByStyle[c.Style][bt] = true
		}
	}
	if len(positions) != 4 {
		t.Errorf("positions = %v, want 4", positions)
	}
	twoPart := 0
	onTypes := map[string]bool{}
	for style, parts := range partsByStyle {
		if len(parts) == 2 {
			twoPart++
		}
		if len(typesByStyle[style]) > 1 {
			t.Errorf("style %s rides %v; a style resolving to several carriers is a different test", style, typesByStyle[style])
		}
		for bt := range typesByStyle[style] {
			onTypes[bt] = true
		}
	}
	if twoPart == 0 {
		t.Error("no two-part style: the running-type rule is not exercised")
	}

	floors := map[string]int{}
	for bt := range onTypes {
		s, ok := p.CarrierSizes[bt]
		if !ok {
			t.Errorf("type %s has no floor size", bt)
			continue
		}
		floors[strings.Join([]string{floorSide(min(s.WidthIn, s.LengthIn)), floorSide(max(s.WidthIn, s.LengthIn))}, "x")]++
	}
	if floors["45x48"] != 3 || floors["30x32"] != 1 {
		t.Errorf("the press's types by floor = %v, want three on 45x48 and one on 30x32", floors)
	}
}

func floorSide(f float64) string {
	return map[float64]string{30: "30", 32: "32", 45: "45", 48: "48"}[f]
}

// The unloaders: four windows, one shared loader, a station holding nothing else
// (which is what makes the board kind grid, so an empty's tap is the panel), an
// outbound of the kanban, and a parts list covering every part the press's styles
// name. The press's fulls go to the windows' group.
func TestKanbanPressPlant_TheUnloader(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress(t)

	stylesOfPress := map[string]bool{}
	for _, s := range p.Styles {
		if s.Process == "KP-PRESS" {
			stylesOfPress[s.Name] = true
		}
	}
	parts := map[string]bool{}
	var windows []Claim
	onStation := map[string][]Claim{}
	for _, c := range p.Claims {
		onStation[c.OperatorStation] = append(onStation[c.OperatorStation], c)
		if stylesOfPress[c.Style] {
			parts[c.Payload] = true
			if c.OutboundDestination != "PRESS-UNLOAD" {
				t.Errorf("press claim %s/%s sends its fulls to %q", c.CoreNode, c.Style, c.OutboundDestination)
			}
		}
		if c.WindowOf == "KP-UNLOADER" {
			windows = append(windows, c)
		}
	}
	if len(windows) != 4 {
		t.Fatalf("windows = %d, want 4", len(windows))
	}
	for _, w := range windows {
		if w.Role != "consume" || !w.IsLoader() || w.InboundSource != "" || w.OutboundDestination != "PRESS-KANBAN" {
			t.Errorf("window %s: %+v, want a consume loader with no inbound and outbound PRESS-KANBAN", w.CoreNode, w)
		}
		for code := range parts {
			found := false
			for _, a := range w.AllowedPayloads {
				found = found || a == code
			}
			if !found {
				t.Errorf("window %s does not list %s", w.CoreNode, code)
			}
		}
		for _, other := range onStation[w.OperatorStation] {
			if other.WindowOf != "KP-UNLOADER" {
				t.Errorf("station %s holds %s beside the windows: its board is no longer all windows", w.OperatorStation, other.CoreNode)
			}
		}
	}
}

// The kanbans, as the brief states them.
func TestKanbanPressPlant_TheKanbans(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress(t)
	byName := map[string]Kanban{}
	for _, g := range p.Kanbans {
		byName[g.Group] = g
	}

	k := byName["PRESS-KANBAN"]
	if k.RunningCover != 4 || k.Strict || k.Overflow != "ZONE-45-SPARE" || strings.Join(k.Sources, ",") != "ZONE-45,ZONE-32,ZONE-45-SPARE" ||
		strings.Join(k.Supports, ",") != "KP-PRESS" || k.Station == "" {
		t.Errorf("PRESS-KANBAN = %+v", k)
	}
	levels := map[string]KanbanLevel{}
	for _, l := range k.Levels {
		levels[l.BinType] = l
	}
	for bt, want := range map[string]int{"SMALL-45": 1, "BIG-45": 1, "TOTES-45": 1, "CART-32": 0} {
		if l, ok := levels[bt]; !ok || l.Want != want {
			t.Errorf("PRESS-KANBAN %s = %+v, want %d", bt, l, want)
		}
	}
	if levels["CART-32"].Overflow != "ZONE-32" {
		t.Errorf("CART-32's own overflow = %q, want ZONE-32", levels["CART-32"].Overflow)
	}

	k2 := byName["WELD-KANBAN"]
	if strings.Join(k2.Sources, ",") != "ZONE-45" || strings.Join(k2.Supports, ",") != "KW-WELD" || k2.Station == "" {
		t.Errorf("WELD-KANBAN = %+v", k2)
	}

	for _, lender := range []string{"ZONE-45", "ZONE-32"} {
		g := byName[lender]
		if len(g.Supports) != 0 || g.Station != "" || g.Strict || len(g.Levels) == 0 {
			t.Errorf("%s = %+v, want levels and no supports, no station, not strict", lender, g)
		}
	}
	if _, levelled := byName["KP-SM"]; levelled {
		t.Error("KP-SM declares a level; R5 needs a lender that cannot be repaid by count")
	}
	if _, levelled := byName["ZONE-45-SPARE"]; levelled {
		t.Error("ZONE-45-SPARE declares a level; it would refuse the carts past the others' levels")
	}
	for _, z := range p.Zones {
		switch z.Name {
		case "ZONE-45-SPARE":
			if len(z.Positions) != 10 || len(z.Lanes) != 0 {
				t.Errorf("ZONE-45-SPARE: %d positions, %d lanes, want 10 flat", len(z.Positions), len(z.Lanes))
			}
		case "PRESS-KANBAN":
			if len(z.Positions) != 8 || len(z.Lanes) != 0 {
				t.Errorf("PRESS-KANBAN: %d positions, %d lanes, want 8 flat", len(z.Positions), len(z.Lanes))
			}
		case "KP-SM":
			if len(z.Lanes) == 0 {
				t.Error("KP-SM has no lanes: it would be a shape a level could be declared on")
			}
		}
	}
}

// A part may go without a carrier only by saying so.
func TestValidate_NoBinTypeIsStatedOrRefused(t *testing.T) {
	t.Parallel()
	base := func() *Plant { return loadKanbanPress(t) }

	blank := base()
	for i := range blank.Payloads {
		if blank.Payloads[i].NoBinType {
			blank.Payloads[i].NoBinType = false
		}
	}
	if err := blank.Validate(); err == nil || !strings.Contains(err.Error(), "unknown bin_type") {
		t.Errorf("a blank bin_type not stated as no_bin_type: %v, want refused", err)
	}

	both := base()
	for i := range both.Payloads {
		if both.Payloads[i].NoBinType {
			both.Payloads[i].BinType = "SMALL-45"
		}
	}
	if err := both.Validate(); err == nil || !strings.Contains(err.Error(), "no_bin_type") {
		t.Errorf("no_bin_type with a bin_type: %v, want refused", err)
	}
}

func TestValidate_CarrierSizes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code string
		size CarrierSize
		want string
	}{
		{"an unknown type", "NOT-A-TYPE", CarrierSize{WidthIn: 48, LengthIn: 45}, "not a bin_type"},
		{"no length", "SMALL-45", CarrierSize{WidthIn: 48}, "width_in and length_in"},
		{"a negative height", "SMALL-45", CarrierSize{WidthIn: 48, LengthIn: 45, HeightIn: -1}, "width_in and length_in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := loadKanbanPress(t)
			p.CarrierSizes[tc.code] = tc.size
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// A shared loader's parts are its allowed list, so each is in the parity count: a
// part a style names and the unloader does not list has no consumer.
func TestValidate_ParityReadsAnUnloadersAllowedList(t *testing.T) {
	t.Parallel()
	p := loadKanbanPress(t)
	for i := range p.Claims {
		if p.Claims[i].WindowOf == "KP-UNLOADER" {
			kept := p.Claims[i].AllowedPayloads[:0:0]
			for _, code := range p.Claims[i].AllowedPayloads {
				if code != "KP-BIG" {
					kept = append(kept, code)
				}
			}
			p.Claims[i].AllowedPayloads = kept
		}
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), `payload "KP-BIG"`) {
		t.Errorf("Validate = %v, want KP-BIG refused as produced with no consumer", err)
	}
}
