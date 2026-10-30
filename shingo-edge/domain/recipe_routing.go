package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"shingoedge/domain/flowspec"
)

// recipe_routing.go — a recipe is choreography; which node fills a role is the
// process's (owner ruling, 2026-10-30).
//
// Positions stay explicit in the recipe. The routing fields resolve BY ROLE
// from the routing set when the recipe is read:
//
//   - one enabled node in the role: the recipe stores the role only (the marker
//     below) and takes whatever Settings names;
//   - several: the recipe stores the node it chose, and only a stored name can
//     orphan;
//   - none: a finding on the card, and the mint refuses by it.
//
// A field naming one of the process's own positions is the recipe's
// choreography and is left alone, whatever the set holds.
//
// A MARKER, NOT A BLANK, because a blank routing field already means something:
// a press with no staging hop is blank there on purpose, and a blank that
// followed Settings would give it one the day the set gained a lane. A marker
// also reads as filled wherever the fields are only asked whether they are
// (RecipeStructureKey, missingRouting). A binary rolled back past this file
// refuses such a recipe by name rather than running it on nothing.
//
// EVERY READER GOES THROUGH ReadRecipe: the card, the preview, the mint, the
// Spots step's lenders and the drift compare. ParseRecipe and ParsePresetShape
// hand back the stored form.
const (
	RecipeFollowsSource      = "@source"
	RecipeFollowsStaging     = "@staging"
	RecipeFollowsDestination = "@destination"
)

// recipeRoutingFields are the routing fields a recipe stores that Settings owns,
// each with its role and marker.
//
// partial_destination IS NOT ONE. It is the one leg that exists to differ from
// the destination, its save does not check it against the routing set, and a
// recipe's partial home can name a node the set has never held. Rewriting it to
// the set's one destination would send unfinished carriers where a full goes.
var recipeRoutingFields = []struct {
	field  flowspec.Field
	role   string
	marker string
	of     func(*FlowCell) *string
}{
	{flowspec.InboundSource, RoutingRoleSource, RecipeFollowsSource, func(c *FlowCell) *string { return &c.InboundSource }},
	{flowspec.InboundStaging, RoutingRoleStaging, RecipeFollowsStaging, func(c *FlowCell) *string { return &c.InboundStaging }},
	{flowspec.OutboundStaging, RoutingRoleStaging, RecipeFollowsStaging, func(c *FlowCell) *string { return &c.OutboundStaging }},
	{flowspec.OutboundDestination, RoutingRoleDestination, RecipeFollowsDestination, func(c *FlowCell) *string { return &c.OutboundDestination }},
	{flowspec.ChangeoverEvacDestination, RoutingRoleDestination, RecipeFollowsDestination, func(c *FlowCell) *string { return &c.ChangeoverEvacDestination }},
}

// isRecipeMarker reports whether v is a role marker rather than a node.
func isRecipeMarker(v string) bool {
	return v == RecipeFollowsSource || v == RecipeFollowsStaging || v == RecipeFollowsDestination
}

// routingFacts is a process's positions and its enabled routing nodes by role,
// gathered once per read.
type routingFacts struct {
	positions map[string]bool
	enabled   map[string][]string
}

func readRoutingFacts(positions []Node, routing []RoutingNode) routingFacts {
	f := routingFacts{positions: map[string]bool{}, enabled: map[string][]string{}}
	for _, p := range positions {
		if p.DeletedAt == nil {
			f.positions[strings.TrimSpace(p.CoreNodeName)] = true
		}
	}
	for _, r := range routing {
		if r.Enabled {
			f.enabled[r.Role] = append(f.enabled[r.Role], strings.TrimSpace(r.CoreNodeName))
		}
	}
	return f
}

// RecipeRead is a recipe as the process's Settings have it now: the cells and
// groups with the routing fields resolved, and every finding, each a sentence
// the card draws and the mint refuses by.
type RecipeRead struct {
	Cells    []FlowCell
	Groups   []RecipeGroup
	Findings []string
}

// ReadRecipe is the one resolver: a stored recipe's routing fields against the
// process's positions and routing set. Pure, and a copy: the caller's rows are
// not touched. See the file's head for the rule.
//
// A MARKER WITH SEVERAL NODES IN ITS ROLE TAKES THE ROLE'S DEFAULT, the row a new
// position opens on (lowestOfRole). The ruling names a single-node role and a
// multi-node recipe, and not a recipe saved under one and read under the other.
// The engineer changing a role adds the new node and takes the old one out, so
// that recipe keeps its node until the old one goes, then follows.
//
// A stored name in a role with no enabled row stays a name: it is live if the
// set still lists it switched off (R4: `enabled` filters what operators are
// offered, not what a flow may name), and stale if it does not.
func ReadRecipe(name string, cells []FlowCell, groups []RecipeGroup, positions []Node, routing []RoutingNode) RecipeRead {
	f := readRoutingFacts(positions, routing)
	out := RecipeRead{Cells: make([]FlowCell, len(cells)), Groups: f.resolveGroups(groups)}
	seen := map[flowspec.Field]bool{}
	for i, c := range cells {
		for _, rf := range recipeRoutingFields {
			p := rf.of(&c)
			v := strings.TrimSpace(*p)
			if v == "" || (f.positions[v] && !isRecipeMarker(v)) {
				continue
			}
			enabled := f.enabled[rf.role]
			switch {
			case len(enabled) == 1:
				*p = enabled[0]
			case !isRecipeMarker(v):
			case len(enabled) > 1:
				*p = lowestOfRole(routing, rf.role)
			default:
				*p = ""
				if !seen[rf.field] {
					seen[rf.field] = true
					out.Findings = append(out.Findings, name+" has no "+strings.ToLower(flowspec.Label(rf.field))+
						" — add one to this process's routing set")
				}
			}
		}
		out.Cells[i] = c
	}
	live := RoutingSetOf(positions, routing)
	for _, st := range append(StaleNodes(out.Cells, live), StaleGroupNodes(out.Groups, live)...) {
		out.Findings = append(out.Findings, StaleSentence(name, st))
	}
	return out
}

// resolveGroups is a recipe's groups with their staging lists read the way its
// cells' staging fields are: a list is the nodes a lane may stand on, so with
// one lane in the role, that lane is the list, and a position in it stays. Any
// other count leaves the list the recipe's own. A copy; the rest of each group
// is as stored.
func (f routingFacts) resolveGroups(groups []RecipeGroup) []RecipeGroup {
	if len(groups) == 0 {
		return nil
	}
	lane := f.enabled[RoutingRoleStaging]
	list := func(nodes []string) []string {
		if len(nodes) == 0 || len(lane) != 1 {
			return nodes
		}
		var out []string
		for _, n := range nodes {
			n = strings.TrimSpace(n)
			switch {
			case f.positions[n]:
				out = append(out, n)
			case !slices.Contains(out, lane[0]):
				out = append(out, lane[0])
			}
		}
		return out
	}
	out := make([]RecipeGroup, len(groups))
	for i, g := range groups {
		spots := make([]GroupSpot, len(g.Spots))
		for j, s := range g.Spots {
			s.InboundStaging, s.OutboundStaging = list(s.InboundStaging), list(s.OutboundStaging)
			spots[j] = s
		}
		g.Spots = spots
		out[i] = g
	}
	return out
}

// CollapseRecipeRouting is the save's half of the rule: a recipe's flow_json with
// every routing field of a one-node role stored as the role only. A position
// stays named, and so does a node of a role with several; the groups stay as
// they are, the reader resolves their lists. A recipe with nothing to collapse
// comes back byte for byte.
func CollapseRecipeRouting(flowJSON string, positions []Node, routing []RoutingNode) (string, error) {
	var env struct {
		Cells  []FlowCell      `json:"cells"`
		Groups json.RawMessage `json:"groups,omitempty"`
	}
	if err := json.Unmarshal([]byte(flowJSON), &env); err != nil {
		return "", fmt.Errorf("%w: %v", ErrFlowPresetMalformed, err)
	}
	f := readRoutingFacts(positions, routing)
	changed := false
	for i := range env.Cells {
		for _, rf := range recipeRoutingFields {
			p := rf.of(&env.Cells[i])
			v := strings.TrimSpace(*p)
			if v == "" || isRecipeMarker(v) || f.positions[v] || len(f.enabled[rf.role]) != 1 {
				continue
			}
			*p, changed = rf.marker, true
		}
	}
	if !changed {
		return flowJSON, nil
	}
	blob, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("marshal recipe: %w", err)
	}
	return string(blob), nil
}

// RecipeOrphans is the recipes whose findings grow between two states of a
// process, by name: what a change to its positions or routing set orphans. rows
// are ListFlowPresets' live rows; only the enabled recipe of each name counts,
// since an older version is on no card. A recipe that does not parse is skipped,
// as the card skips it.
func RecipeOrphans(rows []FlowPreset, positionsBefore, positionsAfter []Node, routingBefore, routingAfter []RoutingNode) []string {
	latest, _ := EnabledRecipes(rows)
	var out []string
	for _, p := range latest {
		cells, groups, err := ParseRecipe(p.FlowJSON)
		if err != nil {
			continue
		}
		before := ReadRecipe(p.Name, cells, groups, positionsBefore, routingBefore).Findings
		after := ReadRecipe(p.Name, cells, groups, positionsAfter, routingAfter).Findings
		for _, sentence := range after {
			if !slices.Contains(before, sentence) {
				out = append(out, p.Name)
				break
			}
		}
	}
	return out
}

// RoutingRemoval is what taking a node out of a routing set left behind: the
// recipes it orphans and the live styles whose claims still name it, with the
// sentence the desktop shows. Empty when it left nothing.
type RoutingRemoval struct {
	Recipes []string `json:"recipes"`
	Styles  []string `json:"styles"`
	Warning string   `json:"warning,omitempty"`
}

// RoutingRemovalWarning is that sentence. A live style keeps routing through the
// node: its claims are its own, so the sentence says so and names them.
func RoutingRemovalWarning(node, role string, recipes, styles []string) string {
	if role == RoutingRoleStaging {
		role = "staging lane"
	}
	var says []string
	if n := len(recipes); n > 0 {
		says = append(says, fmt.Sprintf("Recipes left without their %s (%d): %s.", role, n, andList(quotedNames(recipes, ""))))
	}
	if n := len(styles); n > 0 {
		says = append(says, fmt.Sprintf("Styles still routing through it (%d): %s.", n, andList(quotedNames(styles, ""))))
	}
	if len(says) == 0 {
		return ""
	}
	return "Removed " + node + ". " + strings.Join(says, " ")
}
