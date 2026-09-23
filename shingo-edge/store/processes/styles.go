// styles.go — recipe style persistence inside the processes aggregate.
//
// Phase 6.0c of the architecture refactor folded shingo-edge/store/styles/
// into store/processes/ because styles are part of the process domain
// cluster (process runs a style, style has claims on core nodes, changeover
// transitions between styles). Function names carry the Style suffix so
// they don't collide with the equivalent Process / Node / Claim /
// Changeover functions in their sibling files within this package.

package processes

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// Style represents a product/recipe style that maps to a BOM. The
// struct lives in shingoedge/domain (Stage 2A.2); this alias keeps
// the processes.Style name used by every scan helper, Create/Update
// call site, and the outer store/ re-export.
type Style = domain.Style

// styleSelectColumns is the shared column list for every style SELECT so the
// scan order in scanStyle stays in lockstep. expected_catid is COALESCEd
// because it is added by an idempotent ALTER (older rows land at ”).
const styleSelectColumns = `id, name, description, COALESCE(process_id, 0) as process_id, created_at, COALESCE(expected_catid, '') as expected_catid, deleted_at`

// liveStyles is the WHERE fragment for "styles an operator may still choose".
//
// It is deliberately NOT applied to every read. Of the 23 sites in shingo-edge
// that name the styles table, only six filter, and the split is the whole point
// of soft delete:
//
//   - PICKERS filter. ListStyles, ListStylesByProcess and GetStyleByName feed
//     dropdowns, the changeover target list and name-based ingress. A retired
//     part number must not be selectable, and a tombstone must not resolve by
//     name or a re-created style becomes ambiguous with its own predecessor.
//     Same for the three joins that decide live behaviour rather than render
//     text: claims.IsPairedOnDeckNode, walk.PayloadsForManualSwapNodes and the
//     sim readiness gate.
//
//   - DISPLAY JOINS DO NOT FILTER. The eight LEFT JOIN styles in changeovers.go
//     and the three in counters.go exist to resolve a name or a process_id for
//     a row that already happened. Today a deleted style makes them render a
//     blank name — soft delete is what FIXES that, and filtering would re-break
//     it. GetStyle(id) is in the same category: every caller already holds the
//     id and wants to know what it was.
//
// The rule is: filter where the answer is "what may I pick now", never where
// the answer is "what was this".
const liveStyles = ` deleted_at IS NULL`

func scanStyle(scanner interface{ Scan(...any) error }) (Style, error) {
	var s Style
	var createdAt string
	var deletedAt sql.NullString
	if err := scanner.Scan(&s.ID, &s.Name, &s.Description, &s.ProcessID, &createdAt, &s.ExpectedCATID, &deletedAt); err != nil {
		return s, err
	}
	s.CreatedAt = helpers.ScanTime(createdAt)
	s.DeletedAt = helpers.ScanTimePtr(deletedAt)
	return s, nil
}

func scanStyles(rows *sql.Rows) ([]Style, error) {
	var styles []Style
	for rows.Next() {
		s, err := scanStyle(rows)
		if err != nil {
			return nil, err
		}
		styles = append(styles, s)
	}
	return styles, rows.Err()
}

// ListStyles returns all LIVE styles ordered by name.
//
// PICKER (see liveStyles): this is the whole-plant style list behind
// StyleService().List, the admin styles page and every style dropdown. A
// retired part number must not be selectable.
func ListStyles(db *sql.DB) ([]Style, error) {
	rows, err := db.Query(`SELECT ` + styleSelectColumns + ` FROM styles WHERE` + liveStyles + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// ListStylesByProcess returns the LIVE styles for a single process_id.
//
// PICKER: this is view.AvailableStyles on the operator station — the
// changeover target list — and the claim set plant_claims_publisher pushes to
// Core. Both answer "what may this process run now".
func ListStylesByProcess(db *sql.DB, processID int64) ([]Style, error) {
	rows, err := db.Query(`SELECT `+styleSelectColumns+` FROM styles WHERE process_id = ? AND`+liveStyles+` ORDER BY name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStyles(rows)
}

// GetStyleByName looks up a single LIVE style by name.
//
// PICKER-side: name is an ingress key, and a retired style must not resolve
// through it. Two rows can legitimately share a name once one is retired (the
// live-only unique index permits exactly that), so an unfiltered name lookup
// would be ambiguous rather than merely generous.
func GetStyleByName(db *sql.DB, name string) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT `+styleSelectColumns+` FROM styles WHERE name = ? AND`+liveStyles, name))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetStyle looks up a single style by id, INCLUDING retired ones.
//
// Deliberately unfiltered. Every caller already holds the id — from a
// changeover row, a process's active_style_id, a claim, a CATID monitor — and
// is asking "what is this", not "what may I pick". Filtering here would turn a
// retired style into a nil and reintroduce the blank-name rendering that soft
// delete exists to fix. Callers that need liveness check DeletedAt.
func GetStyle(db DBTX, id int64) (*Style, error) {
	s, err := scanStyle(db.QueryRow(`SELECT `+styleSelectColumns+` FROM styles WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateStyle inserts a new style and returns the new row id.
func CreateStyle(db *sql.DB, name, description string, processID int64) (int64, error) {
	res, err := db.Exec(`INSERT INTO styles (name, description, process_id) VALUES (?, ?, ?)`, name, description, processID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateStyle modifies an existing style.
func UpdateStyle(db *sql.DB, id int64, name, description string, processID int64) error {
	// Guarded on liveness: editing a retired style should not silently succeed.
	_, err := db.Exec(`UPDATE styles SET name=?, description=?, process_id=? WHERE id=? AND`+liveStyles, name, description, processID, id)
	return err
}

// SetStyleExpectedCATID sets (or clears, when empty) the style's expected PLC
// part-identity value. Kept as its own setter rather than a parameter on
// Create/UpdateStyle so the dozens of existing Create/Update call sites stay
// untouched — expected_catid is an independent, optional field the style
// editor writes alongside a save. The value is trimmed by the caller.
func SetStyleExpectedCATID(db *sql.DB, id int64, expectedCATID string) error {
	_, err := db.Exec(`UPDATE styles SET expected_catid=? WHERE id=? AND`+liveStyles, expectedCATID, id)
	return err
}

// DeleteStyle RETIRES a style: it sets deleted_at rather than removing the row.
//
// A hard DELETE cascades into style_node_claims, reporting_points (and from
// there every counter_snapshot beneath them), hourly_counts, payloads,
// and every process_changeover that used this style as its
// TO style — which takes that changeover's node and station tasks with it.
// Measured on the Springfield edge, the worst single style is 91,581 rows. The
// operator who retires a superseded part number is not asking for that, and
// it does not come back.
//
// Retiring instead: the row stays, so nothing that points at it can dangle,
// changeover history keeps rendering a name, and the decision is reversible.
// StyleDeleteImpact reports what a style carries so the confirmation can say
// so.
//
// Its reporting points are DISABLED in the same transaction. That is the one
// piece of the old cascade worth keeping: a retired style must stop counting,
// and rp.enabled is the poll gate (counters.ListEnabledReportingPoints). The
// rows themselves survive, so their snapshots stay attributable.
func DeleteStyle(db *sql.DB, id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE styles SET deleted_at = datetime('now') WHERE id=? AND`+liveStyles, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE reporting_points SET enabled = 0 WHERE style_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RestoreStyle un-retires a style. Soft delete is only worth having if the undo
// exists; without it "reversible" is a claim nobody can act on.
//
// It does NOT re-enable the reporting points: resuming counting is a separate,
// deliberate act, and silently restarting a PLC poll because somebody undid a
// delete is exactly the kind of surprise this whole change is avoiding.
func RestoreStyle(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE styles SET deleted_at = NULL WHERE id=? AND deleted_at IS NOT NULL`, id)
	return err
}

// cloneClaimColumns is the verbatim-copy column list for cloneStyleTx. It
// mirrors UpsertClaim's INSERT in claims.go: a claim column added there MUST be
// added here too, or clones silently drop it. Excludes id (autoincrement),
// style_id (set to the new style) and created_at (defaults to now) — and
// keep_staged, on purpose: a clone takes that column's default, off, because the
// option is withheld (see cloneStyleTx). Kept as a single const so the SELECT and
// INSERT lists can't drift apart from each other.
//
// Attribution is the other deliberate exception: source, called_by and
// updated_at are NOT copied — the clone is its own write and stamps its own
// (see cloneStyleTx) — and retired_at is not copied because only live claims
// are cloned. source_preset_id / _version ARE copied: a clone of a
// preset-built flow is still that preset's shape.
//
// uop_capacity is NOT here, and is not a drop: the column is dead, resolved
// from the payload catalog on read rather than stored (capacity.SQL).
// A clone that copied it would copy a number nothing reads.
const cloneClaimColumns = `core_node_name, role, swap_mode, payload_code,
	reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, containment_destination, allowed_payload_codes, auto_request_payload,
	evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
	lineside_soft_threshold, second_paired_core_node, reuse_compatible_bins, auto_push,
	changeover_evac_nodes, changeover_evac_destination,
	index_robot_supplies, key_route, key_task, changeover_carryover_disposition,
	source_preset_id, source_preset_version`

// cloneStyleTx inserts a new style in src's process and copies every one of
// src's LIVE style_node_claims verbatim, less what the write gate refuses
// (below), within the caller's transaction, stamping the copies with the given
// source ('cloned' / 'generated') and caller. Returns the new style id. Used by
// both CloneStyle (single) and GenerateStyles (batch) so the copy logic lives in
// exactly one place.
func cloneStyleTx(tx *sql.Tx, src *Style, name, description, source, calledBy string) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO styles (name, description, process_id) VALUES (?, ?, ?)`,
		name, description, src.ProcessID)
	if err != nil {
		return 0, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// THE COPY IS VERBATIM EXCEPT FOR WHAT THE WRITE GATE REFUSES. This INSERT
	// never meets UpsertClaim, so the two stored values the gate refuses are left
	// behind here rather than spread to a brand-new style:
	//
	//   - keep_staged set. The option is withheld: UpsertClaim and
	//     ValidateNodeClaim refuse it with domain.KeepStagedWithheld, and the
	//     changeover planner refuses a stored flag the same way, erroring the node
	//     task. The column is out of cloneClaimColumns, so a clone takes its
	//     default, off.
	//   - a manual_swap claim. The mode is retired as a persisted value: it is not
	//     in protocol.ConfigurableSwapModes, so UpsertClaim refuses it, and the
	//     first loader sync quarantines any stored row (QuarantineLoaderClaims). A
	//     row exists only until that sync — unbounded when Core never answers —
	//     and copied in that window it would be a second authority for loader
	//     configuration Core owns, on a style the quarantine has never seen. Those
	//     rows are not selected.
	//   - a RETIRED claim. `liveClaims` is the flow composer's soft delete: a
	//     claim a changeover's history still points at is kept rather than
	//     dropped, and every live read skips it. Cloning one would revive a
	//     deleted claim onto a brand-new style.
	//
	// Pinned by TestCloneStyle_LeavesWithheldConfigurationBehind (store).
	//
	// The copies are ATTRIBUTED to this write rather than carrying src's: a
	// clone is its own act, by whoever asked for it.
	_, err = tx.Exec(`INSERT INTO style_node_claims (style_id, source, called_by, updated_at, `+cloneClaimColumns+`)
		SELECT ?, ?, ?, datetime('now'), `+cloneClaimColumns+` FROM style_node_claims
		WHERE style_id = ? AND swap_mode != ? AND`+liveClaims,
		newID, source, calledBy, src.ID, string(protocol.SwapModeManualSwap))
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// CloneStyle creates a new style in the same process as src, copying src's
// style_node_claims verbatim, less what the write gate refuses (see
// cloneStyleTx). Returns the new style id. The new style
// starts inactive — cloning is a config-time scaffold, not a changeover
// trigger. Operators use this to add a style whose robot choreography matches
// an existing one, then edit only the per-payload fields on the result. The
// copies are attributed source='cloned', called_by=calledBy.
func CloneStyle(db *sql.DB, srcID int64, name, description, calledBy string) (int64, error) {
	src, err := GetStyle(db, srcID)
	if err != nil {
		return 0, err
	}
	if src == nil {
		return 0, fmt.Errorf("source style %d not found", srcID)
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	newID, err := cloneStyleTx(tx, src, name, description, domain.ClaimSourceCloned, calledBy)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newID, nil
}

// GenerateStyles scaffolds a family of styles from one base style in a single
// transaction: each variant is a clone of base with its per-claim payload
// overrides applied (matched by core_node_name). The batch is atomic — a
// duplicate style name or any other error rolls back every variant, so the
// operator never ends up with a half-generated family. Returns the new style
// ids in variant order.
//
// Only payload-shaped fields are overridden (payload_code,
// allowed_payload_codes); the cloned choreography is left untouched, so the
// override can never violate a swap-mode invariant the base already satisfied.
// The capacity that used to be overridden alongside them follows the payload
// by itself now — it is resolved from the catalog, not stored.
// An override whose core_node_name matches no cloned claim updates zero rows
// and is silently skipped — generation is for setting payloads on the base's
// existing claims, not for adding new nodes.
//
// The copies are attributed source='generated', called_by=calledBy.
func GenerateStyles(db *sql.DB, baseID int64, variants []domain.StyleVariant, calledBy string) ([]int64, error) {
	base, err := GetStyle(db, baseID)
	if err != nil {
		return nil, err
	}
	if base == nil {
		return nil, fmt.Errorf("base style %d not found", baseID)
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	ids := make([]int64, 0, len(variants))
	for _, v := range variants {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			return nil, fmt.Errorf("variant name is required")
		}
		newID, err := cloneStyleTx(tx, base, name, strings.TrimSpace(v.Description), domain.ClaimSourceGenerated, calledBy)
		if err != nil {
			return nil, fmt.Errorf("clone variant %q: %w", name, err)
		}
		for _, o := range v.Overrides {
			coreNode := strings.TrimSpace(o.CoreNodeName)
			if coreNode == "" {
				continue
			}
			allowedJSON := marshalAllowedPayloads(o.AllowedPayloadCodes)
			if _, err := tx.Exec(`UPDATE style_node_claims
				SET payload_code=?, allowed_payload_codes=?
				WHERE style_id=? AND core_node_name=?`,
				o.PayloadCode, allowedJSON, newID, coreNode); err != nil {
				return nil, fmt.Errorf("override %s on variant %q: %w", coreNode, name, err)
			}
		}
		ids = append(ids, newID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ClaimOverride is one per-claim adjustment for a CopyStyleClaims batch,
// matched by the SOURCE claim's core_node_name — the copied rows carry the
// source's node names until a rename changes them, which is what makes the
// key stable for the span this struct is used over. Every other field is
// optional: blank inherits the copied claim's value, so a row names only
// what it changes. Shared verbatim with the copy modal, which renders one
// row per source claim with these same fields as inputs.
type ClaimOverride struct {
	Node                 string `json:"node"` // match key; the one required field
	CoreNodeName         string `json:"core_node"`
	Role                 string `json:"role"`
	PayloadCode          string `json:"payload_code"`
	InboundSource        string `json:"inbound_source"`
	OutboundDestination  string `json:"outbound_destination"`
	InboundStaging       string `json:"inbound_staging"`
	OutboundStaging      string `json:"outbound_staging"`
	PairedCoreNode       string `json:"paired_core_node"`
	SecondPairedCoreNode string `json:"second_paired_core_node"`
}

// copiedClaim is the slice of a freshly copied claim row the override layer
// reads and rewrites. Read once up front instead of per-override, and
// updated in memory as each write lands, so collision and distinctness
// checks in the rename pass see the same truth the earlier writes produced.
type copiedClaim struct {
	node                               string
	swapMode                           protocol.SwapMode
	payload                            string
	allowed                            []string
	inboundSource, outboundDestination string
	inboundStaging, outboundStaging    string
	paired, second                     string
}

// readCopiedClaims loads the just-inserted copies for the override layer.
// allowed_payload_codes tolerates the column's legacy empty default: an
// unparseable or empty value reads as no allowed list, which only ever
// widens what the payload-append pass below writes back.
func readCopiedClaims(tx *sql.Tx, targetID int64) (map[string]*copiedClaim, error) {
	gr, err := tx.Query(`SELECT core_node_name, swap_mode, payload_code, allowed_payload_codes,
		inbound_source, outbound_destination, inbound_staging, outbound_staging,
		paired_core_node, second_paired_core_node FROM style_node_claims WHERE style_id = ? AND`+liveClaims, targetID)
	if err != nil {
		return nil, err
	}
	defer gr.Close()
	out := map[string]*copiedClaim{}
	for gr.Next() {
		var c copiedClaim
		var allowed string
		if err := gr.Scan(&c.node, &c.swapMode, &c.payload, &allowed, &c.inboundSource,
			&c.outboundDestination, &c.inboundStaging, &c.outboundStaging, &c.paired, &c.second); err != nil {
			return nil, err
		}
		var allowedList []string
		_ = json.Unmarshal([]byte(allowed), &allowedList)
		c.allowed = allowedList
		out[c.node] = &c
	}
	return out, gr.Err()
}

// overrideApplied returns the claim's values as one override row leaves
// them. Blank fields survive untouched; text fields are trimmed on the same
// trust as UpsertClaim trims again behind the API's trim. Role is not here:
// the requirement rules key on the claim's swap mode, never its role, so
// the role gate reads the final values and keeps its own decision separate.
func overrideApplied(row copiedClaim, ov ClaimOverride) copiedClaim {
	out := row
	if v := strings.TrimSpace(ov.PayloadCode); v != "" {
		out.payload = v
	}
	if v := strings.TrimSpace(ov.InboundSource); v != "" {
		out.inboundSource = v
	}
	if v := strings.TrimSpace(ov.OutboundDestination); v != "" {
		out.outboundDestination = v
	}
	if v := strings.TrimSpace(ov.InboundStaging); v != "" {
		out.inboundStaging = v
	}
	if v := strings.TrimSpace(ov.OutboundStaging); v != "" {
		out.outboundStaging = v
	}
	if v := strings.TrimSpace(ov.PairedCoreNode); v != "" {
		out.paired = v
	}
	if v := strings.TrimSpace(ov.SecondPairedCoreNode); v != "" {
		out.second = v
	}
	return out
}

// claimSwapModeRequirement restates processes/claims.go UpsertClaim's own
// SwapMode rules against a claim's final values, for the override layer's
// validity check. Same rules, same order, same wording. The copy path
// trusts live claims as they are, so the only route to a violated rule here
// is an override that was supposed to supply the missing field and didn't.
// Empty message = the claim passes.
func claimSwapModeRequirement(mode protocol.SwapMode, c copiedClaim) string {
	switch mode {
	case protocol.SwapModeManualSwap:
		if c.outboundDestination == "" {
			return "manual_swap claims require outbound_destination to be set"
		}
	case protocol.SwapModeTwoRobot:
		if c.inboundStaging == "" {
			return "two_robot claims require inbound_staging to be set"
		}
	case protocol.SwapModeTwoRobotPressIndex:
		if c.paired == "" {
			return "two_robot_press_index claims require paired_core_node (back position) to be set"
		}
		if c.outboundDestination == "" {
			return "two_robot_press_index claims require outbound_destination to be set"
		}
	}
	return ""
}

// applyClaimOverrides rewrites the freshly copied target claims per the
// operator's adjust rows, in two passes inside the copy transaction.
//
// PASS 1 — field updates, keyed on the claim's current (source) node name.
// Role is the only field with a validity gate, and the gate is the one
// agreed for this feature: the SwapMode requirements are evaluated against
// the claim's POST-override values (the row may supply the missing field
// beside the role it changes), and a claim that still fails one keeps its
// copied role while every other field of that row still applies. A
// two_robot_press_index paired/second override is distinctness-checked the
// same way and withheld alone — the front, back and third positions must
// stay distinct (claims.go:246-252).
//
// PASS 2 — renames, after the fields, because a rename changes the match
// key pass 1 keyed on. A rename is refused (with a note, not an error —
// UNIQUE(style_id, core_node_name) would abort the whole batch otherwise)
// when the copied set already holds a claim on the new name, or when the
// renamed claim itself is a press-index cell whose positions the rename
// would collapse onto each other. A rename that lands ALSO repairs the
// pairing partner: every PairedCoreNode / SecondPairedCoreNode in the
// target still holding the old name is pointed at the new one in the same
// transaction — an A/B pair copied with one side renamed must not end up
// referencing a node that no longer has a claim.
//
// Notes are returned to the caller for the operator's results toast.
func applyClaimOverrides(tx *sql.Tx, targetID int64, overrides []ClaimOverride) ([]string, error) {
	notes := make([]string, 0, len(overrides))
	if len(overrides) == 0 {
		return notes, nil
	}
	rows, err := readCopiedClaims(tx, targetID)
	if err != nil {
		return nil, fmt.Errorf("read copied claims for overrides: %w", err)
	}

	for _, ov := range overrides {
		node := strings.TrimSpace(ov.Node)
		if node == "" {
			notes = append(notes, `an override row is missing its "node" match key — ignored`)
			continue
		}
		row := rows[node]
		if row == nil {
			notes = append(notes, fmt.Sprintf("node %q: the source style has no such claim — override ignored", node))
			continue
		}

		var sets []string
		var args []any
		add := func(col, val string) {
			sets = append(sets, col+" = ?")
			args = append(args, val)
		}
		if v := strings.TrimSpace(ov.PayloadCode); v != "" {
			add("payload_code", v)
			row.payload = v
			// Keep the allowed list coherent with the payload: sourcing
			// (walk.go) checks the requested payload against
			// claim.AllowedPayloads(), so a payload outside the list makes
			// the claim unsourceable — silently, exactly the failure shape
			// this layer exists to avoid shipping.
			if !slices.Contains(row.allowed, v) && !slices.Contains(row.allowed, "*") {
				row.allowed = append(slices.Clone(row.allowed), v)
				add("allowed_payload_codes", marshalAllowedPayloads(row.allowed))
			}
		}
		if v := strings.TrimSpace(ov.InboundSource); v != "" {
			add("inbound_source", v)
			row.inboundSource = v
		}
		if v := strings.TrimSpace(ov.OutboundDestination); v != "" {
			add("outbound_destination", v)
			row.outboundDestination = v
		}
		if v := strings.TrimSpace(ov.InboundStaging); v != "" {
			add("inbound_staging", v)
			row.inboundStaging = v
		}
		if v := strings.TrimSpace(ov.OutboundStaging); v != "" {
			add("outbound_staging", v)
			row.outboundStaging = v
		}
		// Paired-position overrides on a press-index claim answer to the
		// same validity rule the rename pass enforces: front, back and third
		// must stay distinct. The offending field is withheld alone, noted,
		// and everything else in the row still applies.
		if v := strings.TrimSpace(ov.PairedCoreNode); v != "" {
			if row.swapMode == protocol.SwapModeTwoRobotPressIndex && v == node {
				notes = append(notes, fmt.Sprintf("node %q: paired_core_node override refused — it names the claim's own front position", node))
			} else {
				add("paired_core_node", v)
				row.paired = v
			}
		}
		if v := strings.TrimSpace(ov.SecondPairedCoreNode); v != "" {
			if row.swapMode == protocol.SwapModeTwoRobotPressIndex && (v == node || v == row.paired) {
				notes = append(notes, fmt.Sprintf("node %q: second_paired_core_node override refused — a press-index claim's positions must stay distinct", node))
			} else {
				add("second_paired_core_node", v)
				row.second = v
			}
		}
		// The role override answers to the SwapMode requirements, evaluated
		// against the claim's post-override values — supplying the missing
		// field beside the role change makes it legal. A claim that still
		// fails keeps its copied role; the other fields apply regardless.
		if s := strings.TrimSpace(ov.Role); s != "" {
			want := protocol.ClaimRole(s)
			if want != protocol.ClaimRoleProduce {
				want = protocol.ClaimRoleConsume
			}
			if msg := claimSwapModeRequirement(row.swapMode, overrideApplied(*row, ov)); msg != "" {
				notes = append(notes, fmt.Sprintf("node %q: role change withheld — %s", node, msg))
			} else {
				add("role", string(want))
			}
		}
		if len(sets) > 0 {
			args = append(args, targetID, node)
			if _, err := tx.Exec(`UPDATE style_node_claims SET `+strings.Join(sets, ", ")+`
				WHERE style_id = ? AND core_node_name = ?`, args...); err != nil {
				return nil, fmt.Errorf("override node %q: %w", node, err)
			}
		}
	}

	// PASS 2 — renames, after the fields, because a rename changes the match
	// key pass 1 keyed on.
	for _, ov := range overrides {
		oldName := strings.TrimSpace(ov.Node)
		newName := strings.TrimSpace(ov.CoreNodeName)
		if oldName == "" || newName == "" || newName == oldName {
			continue
		}
		row := rows[oldName]
		if row == nil {
			continue // already noted in pass 1
		}
		if clash := rows[newName]; clash != nil {
			notes = append(notes, fmt.Sprintf("node %q: rename to %q refused — the copied set already has a claim on %q", oldName, newName, newName))
			continue
		}
		if row.swapMode == protocol.SwapModeTwoRobotPressIndex && (newName == row.paired || (row.second != "" && newName == row.second)) {
			notes = append(notes, fmt.Sprintf("node %q: rename to %q refused — a two_robot_press_index claim's positions must stay distinct", oldName, newName))
			continue
		}
		if _, err := tx.Exec(`UPDATE style_node_claims SET core_node_name = ?
			WHERE style_id = ? AND core_node_name = ?`, newName, targetID, oldName); err != nil {
			return nil, fmt.Errorf("rename override %q: %w", oldName, err)
		}
		// Auto-fix the pairing partner: the operator renamed one side of an
		// A/B pair, so references still pointing at the old name follow it.
		// Rows affected are reported, because a cascade nobody can see is
		// indistinguishable from a rename that never happened.
		for _, col := range []string{"paired_core_node", "second_paired_core_node"} {
			res, err := tx.Exec(`UPDATE style_node_claims SET `+col+` = ?
				WHERE style_id = ? AND `+col+` = ?`, newName, targetID, oldName)
			if err != nil {
				return nil, fmt.Errorf("rename cascade %q: %w", oldName, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				notes = append(notes, fmt.Sprintf("rename %q → %q: %d claim(s)' %s reference updated", oldName, newName, n, col))
			}
		}
		delete(rows, oldName)
		row.node = newName
		rows[newName] = row
	}
	return notes, nil
}

// CopyStyleClaims replaces target's node claims with src's, within one
// transaction: the clone column list, verbatim. Used by the "Copy Node
// Claims" action to push one style's choreography onto sibling styles.
//
// includePayloads=false preserves the TARGET's payloads: its per-node
// payload map is snapshotted before the delete and reapplied after the
// insert, for nodes the two styles share. A target node the source style
// doesn't have is gone with the rest of the replace; a node new to the
// target arrives with the source's payload — there was nothing of the
// target's to preserve. Callers enforce the active-style and same-process
// rules; this function is the mechanical replace.
//
// overrides is the optional per-claim adjust layer, applied AFTER the copy
// and after the payload snapshot reapply (an operator asking for a specific
// payload outranks the keep-mine rule). It returns human-readable notes —
// overrides it could not match, renames it refused, pair references it
// auto-fixed, role changes it withheld — because a batch that silently
// drops part of what was asked for is how the next incident starts.
func CopyStyleClaims(db *sql.DB, srcID, targetID int64, includePayloads bool, overrides []ClaimOverride) ([]string, error) {
	if srcID == targetID {
		return nil, fmt.Errorf("source and target are the same style")
	}
	if _, err := GetStyle(db, srcID); err != nil {
		return nil, fmt.Errorf("source style: %w", err)
	}
	tgt, err := GetStyle(db, targetID)
	if err != nil {
		return nil, fmt.Errorf("target style: %w", err)
	}
	if tgt == nil {
		return nil, fmt.Errorf("target style %d not found", targetID)
	}

	// The target's payloads per node — snapshotted before the replace when
	// they must survive it. The bulk insert copies the source's payloads
	// too (full clone column list); the snapshot is then layered back over
	// the nodes the target owned, so "exclude payloads" means "keep mine",
	// not "arrive empty".
	savedPayloads := map[string]string{}
	if !includePayloads {
		claims, err := ListClaims(db, targetID)
		if err != nil {
			return nil, fmt.Errorf("read target claims: %w", err)
		}
		for _, c := range claims {
			if c.PayloadCode != "" {
				savedPayloads[c.CoreNodeName] = c.PayloadCode
			}
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM style_node_claims WHERE style_id = ?`, targetID); err != nil {
		return nil, err
	}
	// swap_mode is copied verbatim on the same trust as cloneStyleTx: live
	// claims already hold a configurable mode, nothing stale to re-validate.
	if _, err := tx.Exec(`INSERT INTO style_node_claims (style_id, `+cloneClaimColumns+`)
		SELECT ?, `+cloneClaimColumns+` FROM style_node_claims WHERE style_id = ? AND`+liveClaims,
		targetID, srcID); err != nil {
		return nil, err
	}
	if !includePayloads {
		for node, payload := range savedPayloads {
			if _, err := tx.Exec(`UPDATE style_node_claims SET payload_code = ?
				WHERE style_id = ? AND core_node_name = ?`, payload, targetID, node); err != nil {
				return nil, err
			}
		}
	}
	notes, err := applyClaimOverrides(tx, targetID, overrides)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return notes, nil
}

// StyleImpact is what a style is carrying, counted so a confirmation dialog can
// say it out loud.
//
// Every field is a row count that a HARD delete of this style would destroy
// under the schema's declared foreign-key actions. DeleteStyle does not do a
// hard delete — it retires the row — so these are not losses. They are the
// answer to "what am I retiring", and they are also exactly what a future purge
// would cost, which is the number nobody had before.
//
// The shape of this matters more than the total. On the Springfield edge the
// median style carries one row (itself) and the worst carries 91,581, of which
// 91,256 are raw counter snapshots. A dialog that says "delete this style?" is
// asking the same question in both cases and they are not the same question.
type StyleImpact struct {
	StyleID int64  `json:"style_id"`
	Name    string `json:"name"`
	Retired bool   `json:"retired"`

	Claims          int `json:"claims"`
	ReportingPoints int `json:"reporting_points"`
	Snapshots       int `json:"counter_snapshots"`
	HourlyCounts    int `json:"hourly_counts"`
	PartsCounted    int `json:"parts_counted"`
	ChangeoversTo   int `json:"changeovers_to"`
	NodeTasks       int `json:"changeover_node_tasks"`
	StationTasks    int `json:"changeover_station_tasks"`
	Participants    int `json:"changeover_participants"`
	Payloads        int `json:"payloads"`

	// ChangeoversFrom is SET NULL, not CASCADE: those changeover rows survive a
	// hard delete and merely lose the pointer that says what they came from.
	// Reported separately because "60 rows lose a field" and "60 rows cease to
	// exist" should never share a total.
	ChangeoversFrom int `json:"changeovers_from_detached"`

	// TotalDeleted is the sum of the CASCADE side plus the style row itself.
	TotalDeleted int `json:"total_deleted"`
}

// StyleDeleteImpact counts everything a hard delete of styleID would take.
//
// It walks the cascade explicitly rather than trusting the declared actions to
// be what anybody expects, because on this schema they are not: styles ->
// reporting_points -> counter_snapshots is two CASCADE hops, and the second one
// is where the volume lives. Two hops is exactly far enough that nobody reading
// the styles table would guess the number.
func StyleDeleteImpact(db *sql.DB, styleID int64) (*StyleImpact, error) {
	imp := &StyleImpact{StyleID: styleID}
	var deletedAt sql.NullString
	err := db.QueryRow(`SELECT name, deleted_at FROM styles WHERE id = ?`, styleID).Scan(&imp.Name, &deletedAt)
	if err != nil {
		return nil, err
	}
	imp.Retired = deletedAt.Valid

	for _, q := range []struct {
		dst *int
		sql string
	}{
		{&imp.Claims, `SELECT count(*) FROM style_node_claims WHERE style_id = ? AND retired_at IS NULL`},
		{&imp.ReportingPoints, `SELECT count(*) FROM reporting_points WHERE style_id = ?`},
		{&imp.Snapshots, `SELECT count(*) FROM counter_snapshots
			WHERE reporting_point_id IN (SELECT id FROM reporting_points WHERE style_id = ?)`},
		{&imp.HourlyCounts, `SELECT count(*) FROM hourly_counts WHERE style_id = ?`},
		{&imp.PartsCounted, `SELECT COALESCE(SUM(delta), 0) FROM hourly_counts WHERE style_id = ?`},
		{&imp.ChangeoversTo, `SELECT count(*) FROM process_changeovers WHERE to_style_id = ?`},
		{&imp.NodeTasks, `SELECT count(*) FROM changeover_node_tasks
			WHERE process_changeover_id IN (SELECT id FROM process_changeovers WHERE to_style_id = ?)`},
		{&imp.StationTasks, `SELECT count(*) FROM changeover_station_tasks
			WHERE process_changeover_id IN (SELECT id FROM process_changeovers WHERE to_style_id = ?)`},
		{&imp.Participants, `SELECT count(*) FROM changeover_participants
			WHERE process_changeover_id IN (SELECT id FROM process_changeovers WHERE to_style_id = ?)`},
		{&imp.Payloads, `SELECT count(*) FROM payloads WHERE job_style_id = ?`},
		{&imp.ChangeoversFrom, `SELECT count(*) FROM process_changeovers WHERE from_style_id = ?`},
	} {
		if err := db.QueryRow(q.sql, styleID).Scan(q.dst); err != nil {
			// A table absent on this vintage counts as zero rather than failing
			// the whole confirmation: refusing to show a dialog because a
			// table does not exist yet would be worse than showing one
			// number short.
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return nil, fmt.Errorf("style %d impact: %w", styleID, err)
		}
	}
	imp.TotalDeleted = 1 + imp.Claims + imp.ReportingPoints + imp.Snapshots + imp.HourlyCounts +
		imp.ChangeoversTo + imp.NodeTasks + imp.StationTasks + imp.Participants +
		imp.Payloads
	return imp, nil
}
