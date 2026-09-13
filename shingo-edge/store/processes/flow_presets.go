// flow_presets.go — named flows saved per process, inside the processes
// aggregate. See domain/flow_preset.go for what a preset is and what it may
// not carry. Store and validation only; the UI and endpoints are a later unit.

package processes

import (
	"database/sql"
	"fmt"
	"strings"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// FlowPreset and FlowPresetInput are the preset data types; the structs live
// in shingoedge/domain.
type (
	FlowPreset      = domain.FlowPreset
	FlowPresetInput = domain.FlowPresetInput
)

const flowPresetSelect = `id, process_id, name, version, flow_json, created_by, created_at, archived_at`

func scanFlowPreset(scanner interface{ Scan(...any) error }) (FlowPreset, error) {
	var p FlowPreset
	var createdAt string
	var archivedAt sql.NullString
	if err := scanner.Scan(&p.ID, &p.ProcessID, &p.Name, &p.Version, &p.FlowJSON, &p.CreatedBy, &createdAt, &archivedAt); err != nil {
		return p, err
	}
	p.CreatedAt = helpers.ScanTime(createdAt)
	p.ArchivedAt = helpers.ScanTimePtr(archivedAt)
	return p, nil
}

// ListFlowPresets returns a process's presets, newest version of each name
// first. Archived versions are hidden unless includeArchived — they are
// still resolvable by id, because claim provenance points at them.
func ListFlowPresets(db *sql.DB, processID int64, includeArchived bool) ([]FlowPreset, error) {
	where := ` WHERE process_id = ?`
	if !includeArchived {
		where += ` AND archived_at IS NULL`
	}
	rows, err := db.Query(`SELECT `+flowPresetSelect+` FROM flow_presets`+where+` ORDER BY name, version DESC`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowPreset
	for rows.Next() {
		p, err := scanFlowPreset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFlowPreset returns one preset by id, archived or not.
func GetFlowPreset(db *sql.DB, id int64) (*FlowPreset, error) {
	p, err := scanFlowPreset(db.QueryRow(`SELECT `+flowPresetSelect+` FROM flow_presets WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateFlowPreset validates and saves a preset as the NEXT version of its
// name in its process — a preset is never edited in place, because claims
// record which version they came from. Returns the new row id.
//
// The node universe the flow is checked against is the process's positions
// plus EVERY member of its routing set, adopted or not (owner ruling R4):
// `enabled` filters what operators are offered, not what a flow may name, and
// a preset validation stricter than `flow/save` refuses to name flows the
// press already runs. See RoutingSetNames.
func CreateFlowPreset(db *sql.DB, in FlowPresetInput) (int64, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return 0, fmt.Errorf("preset name is required")
	}
	allowed, err := RoutingSetNames(db, in.ProcessID)
	if err != nil {
		return 0, err
	}
	if err := domain.ValidateFlowPreset(in.FlowJSON, allowed); err != nil {
		return 0, err
	}
	if err := refuseNameOfAnotherShape(db, in); err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO flow_presets (process_id, name, version, flow_json, created_by)
		SELECT ?1, ?2, COALESCE(MAX(version), 0) + 1, ?3, ?4 FROM flow_presets WHERE process_id = ?1 AND name = ?2`,
		in.ProcessID, in.Name, in.FlowJSON, in.CreatedBy)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// refuseNameOfAnotherShape is owner ruling F2 (2026-09-12): a repeated name is
// a new VERSION of one shape, never a second shape under one name.
//
// It compares against the name's NEWEST version, which is what the lineage
// currently means — an older version is the same lineage at an earlier point,
// and a shape that has been edited twice would otherwise be refused its own
// third version. Archived rows count: archiving hides a version from the
// chooser and leaves every member's provenance pointing at it, so the name is
// still spoken for.
//
// A shape that does not parse is not a shape to compare against, and the
// refusal would then be about a row nobody can read. That row is already
// logged and skipped everywhere it is offered (composerPresets), so this lets
// the new version through rather than blocking a name on a broken row.
func refuseNameOfAnotherShape(db *sql.DB, in FlowPresetInput) error {
	var flowJSON string
	err := db.QueryRow(`SELECT flow_json FROM flow_presets
		WHERE process_id = ? AND name = ? ORDER BY version DESC LIMIT 1`,
		in.ProcessID, in.Name).Scan(&flowJSON)
	if err == sql.ErrNoRows {
		return nil // a name nobody holds
	}
	if err != nil {
		return err
	}
	held, err := domain.ParsePresetShape(flowJSON)
	if err != nil {
		return nil // unreadable: not a shape to be different from
	}
	want, err := domain.ParsePresetShape(in.FlowJSON)
	if err != nil {
		return nil // ValidateFlowPreset above already refused this
	}
	if domain.FlowShapeKey(held) == domain.FlowShapeKey(want) {
		return nil // the same shape, changing: version n+1, as before
	}
	return fmt.Errorf("%q %w", in.Name, domain.ErrFlowPresetNameIsAnotherShape)
}

// RenameFlowPreset renames EVERY VERSION of a preset's name in its process,
// and changes nothing else (owner ruling R6, 2026-09-12).
//
// EVERY VERSION, because a name is not a property of a version. UNIQUE is per
// (process, name, version), so renaming one row of a two-version name would
// leave two presets that mean one shape's history — `2‑robot index v1` and
// whatever v2 was renamed to — with no way to tell from either list that they
// are the same lineage. The id is what provenance points at and the id does
// not move, so a member claim's source_preset_id keeps resolving to the same
// version of the same shape under its new name.
//
// A name already in use by ANOTHER lineage is refused by name rather than
// merged into it: two lineages under one name would interleave versions and a
// v2 would mean a different shape from its own v1.
func RenameFlowPreset(db *sql.DB, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("preset name is required")
	}
	p, err := GetFlowPreset(db, id)
	if err != nil {
		return err
	}
	if p.Name == name {
		return nil
	}
	var clash int
	if err := db.QueryRow(`SELECT COUNT(*) FROM flow_presets WHERE process_id = ? AND name = ?`,
		p.ProcessID, name).Scan(&clash); err != nil {
		return err
	}
	if clash > 0 {
		return fmt.Errorf("this press already has a preset called %q", name)
	}
	_, err = db.Exec(`UPDATE flow_presets SET name = ? WHERE process_id = ? AND name = ?`,
		name, p.ProcessID, p.Name)
	return err
}

// ArchiveFlowPreset hides a preset version from the chooser. The row stays,
// because a claim's provenance may point at it.
func ArchiveFlowPreset(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE flow_presets SET archived_at = datetime('now') WHERE id = ? AND archived_at IS NULL`, id)
	return err
}
