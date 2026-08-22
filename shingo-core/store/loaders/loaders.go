// Package loaders holds the Core-owned bin-loader aggregate (loader refactor
// cutover): bin_loaders + bin_loader_homes + bin_loader_payloads. The loader's
// identity and per-position/per-payload replenishment config live here, on
// Core, replacing the Edge style_node_claims encoding.
//
// Free functions over *sql.DB mirror the store/demands sub-package; the outer
// store/ keeps one-line delegate methods on *store.DB (store/loaders.go).
//
// These reads/writes back the Core-owned loader read path: the aggregate the
// Edge syncs into its core_loaders cache and resolves loaders from.
package loaders

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"shingo/protocol"
)

// Key mints the opaque wire/identity token for a loader from its surrogate id:
// "loader:<id>". The constant prefix keeps it obviously-a-loader and greppable and
// guarantees it never collides with a node name (the identity-as-node trap). There is
// NO plant/factory prefix — cross-plant federation is not a goal, so no per-plant
// config value is needed (decision §0-10). Core is the sole minter; Edge propagates
// the token off the wire (LoaderInfo.LoaderKey) and never re-derives it.
func Key(id int64) string { return "loader:" + strconv.FormatInt(id, 10) }

// Enum values: layout = shared_window | dedicated_positions; role = produce |
// consume; replenishment = operator | threshold. A produce loader is operator
// (operator stages/clears at the board) or threshold (UOP kanban autoreorder).
// A consume loader (unloader) is always operator — the window-queue drain.
// "auto" was renamed to "threshold" (v40 migration) once the legacy bin-count
// floor was retired, so "auto" no longer conflates threshold with bin-count.
//
// The strings themselves are single-sourced in protocol/: these values cross
// the wire, and Edge spells the same vocabulary in shingo-edge/domain, so a
// disagreement is a defect that reaches the floor rather than a style
// difference. The names and the local surface are unchanged.
const (
	RoleProduce = string(protocol.ClaimRoleProduce)
	RoleConsume = string(protocol.ClaimRoleConsume)

	LayoutSharedWindow       = protocol.LoaderLayoutSharedWindow
	LayoutDedicatedPositions = protocol.LoaderLayoutDedicatedPositions

	ReplenishmentOperator  = protocol.LoaderReplenishmentOperator
	ReplenishmentThreshold = protocol.LoaderReplenishmentThreshold

	// home_kind discriminates a dedicated loader's members: a HOME is a position
	// the cell binds to (payload pinned, or blank when not yet assigned); a BUFFER
	// is a kept-partial slot with no pinned payload. Source ranks homes ∪ buffers;
	// an unpinned home (kind=home, blank payload) is inert. Replaces the
	// blank-payload overload (D4 / round-3 Call 2).
	HomeKindHome   = protocol.LoaderHomeKindHome
	HomeKindBuffer = protocol.LoaderHomeKindBuffer
)

// Loader is the aggregate root: a bin loader (produce) or unloader (consume)
// anchored at a core node. config_gen is bumped on every config-affecting write
// and rides the downward sync so Edge can detect stale config.
type Loader struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	Layout        string     `json:"layout"`
	Replenishment string     `json:"replenishment"`
	OutboundDest  string     `json:"outbound_dest"`
	InboundSource string     `json:"inbound_source"`
	ConfigGen     int64      `json:"config_gen"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"` // soft-delete marker; nil = active (step 7)

	// FunnelWindows restricts a shared-window loader to ONE window at a time:
	// inbound empties all go to its first window on a budget of 1, instead of
	// spreading one bin per window across a budget of the window count.
	//
	// This replaces the plant-wide Edge config key `loaders_multi_window`, which
	// could only answer for every loader at once, so a plant needing the funnel
	// for one loader imposed it on all of them. How a loader is fed is a fact
	// about that loader, not about the site.
	//
	// STATED AS THE RESTRICTION rather than as "multi_window", so that FALSE --
	// the Go zero value, the column default, the value an older Core omits from
	// the wire, and the value a bare struct literal carries -- all mean "spread",
	// which is what every loader does today. The inverted name costs one negation
	// at the read site and saves a special case at each of the five places a
	// default could otherwise be silently wrong.
	//
	// IGNORED by dedicated_positions loaders: their positions are independent
	// one-bin slots that never shared a budget, so there is nothing to funnel.
	FunnelWindows bool `json:"funnel_windows"`
}

// Home is one dedicated position: exactly one payload. The global
// UNIQUE(position_node_id) makes "one payload per position, one loader per node"
// unrepresentable-otherwise — the structural fix for the SLN_002 incident.
//
// The legacy bin-count floor (min_stock) is retired: a loader is operator-driven
// or UOP-threshold-driven, never bin-count-driven. The DB column is left dormant
// (defaults to 0) and is no longer read or written.
type Home struct {
	LoaderID       int64  `json:"loader_id"`
	PositionNodeID int64  `json:"position_node_id"`
	PayloadCode    string `json:"payload_code"`
	Kind           string `json:"home_kind"` // HomeKindHome | HomeKindBuffer; "" normalises to home
	UOPThreshold   int    `json:"uop_threshold"`
	SortOrder      int    `json:"sort_order"`
}

// InSourcePool reports whether this member's bins belong in the loader's Source
// candidate pool. A BUFFER slot always does (it holds kept partials). A pinned
// HOME does. An UNPINNED home (dragged in, no payload assigned yet) does NOT — it
// is inert, so a stray bin parked on a half-configured position is never sourced.
// This is the D4 disambiguation: buffer vs unassigned-home, keyed on home_kind,
// not on the overloaded blank payload. (A blank kind reads as home — see UpsertHome.)
func (h Home) InSourcePool() bool {
	return h.Kind == HomeKindBuffer || h.PayloadCode != ""
}

// Payload is one entry in a shared_window loader's allowed set.
type Payload struct {
	LoaderID     int64  `json:"loader_id"`
	PayloadCode  string `json:"payload_code"`
	UOPThreshold int    `json:"uop_threshold"`
}

// Config bundles a loader with its positions (dedicated_positions) and/or
// payload set (shared_window) — the assembled read the downward sync and the
// future runtime LoaderStore consume.
type Config struct {
	Loader   Loader    `json:"loader"`
	Homes    []Home    `json:"homes"`
	Payloads []Payload `json:"payloads"`
}

const loaderCols = `id, name, role, layout, replenishment, outbound_dest, inbound_source, config_gen, archived_at, funnel_windows`

type scanner interface{ Scan(...any) error }

func scanLoader(s scanner) (Loader, error) {
	var l Loader
	var archivedAt sql.NullTime
	err := s.Scan(&l.ID, &l.Name, &l.Role, &l.Layout, &l.Replenishment,
		&l.OutboundDest, &l.InboundSource, &l.ConfigGen, &archivedAt, &l.FunnelWindows)
	if archivedAt.Valid {
		l.ArchivedAt = &archivedAt.Time
	}
	return l, err
}

// CreateLoader inserts a loader and returns its id. The surrogate id IS the loader's
// identity (minted onto the wire as the loader_key token); role + layout are fixed
// after creation.
func CreateLoader(db *sql.DB, l Loader) (int64, error) {
	var id int64
	err := db.QueryRow(`
		INSERT INTO bin_loaders (name, role, layout, replenishment, outbound_dest, inbound_source, funnel_windows)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		l.Name, l.Role, l.Layout, l.Replenishment, l.OutboundDest, l.InboundSource, l.FunnelWindows,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create loader %q: %w", l.Name, err)
	}
	return id, nil
}

// GetLoader returns the loader by id, or (nil, nil) if absent.
func GetLoader(db *sql.DB, id int64) (*Loader, error) {
	l, err := scanLoader(db.QueryRow(`SELECT `+loaderCols+` FROM bin_loaders WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get loader %d: %w", id, err)
	}
	return &l, nil
}

// GetLoaderByName returns the loader named (name, role), or (nil, nil) if absent.
// (name, role) is the seed / migrateloaders idempotency key now that identity is the
// surrogate id: a re-run finds the existing loader by its stable operator-facing name
// rather than the dropped core_node_name.
func GetLoaderByName(db *sql.DB, name, role string) (*Loader, error) {
	l, err := scanLoader(db.QueryRow(`SELECT `+loaderCols+` FROM bin_loaders WHERE name=$1 AND role=$2`, name, role))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get loader by name %s/%s: %w", name, role, err)
	}
	return &l, nil
}

// ListLoaders returns every ACTIVE loader (archived_at IS NULL), ordered by name. This
// is the config enumeration the downward sync (BuildLoaderInfos) and demand derivation
// (BuildDemandRegistryFromAggregate) consume, so a soft-deleted loader stops driving the
// plant. Analytics that must include retired loaders read bin_uop_ledger (the stamped
// loader_id survives), not this.
func ListLoaders(db *sql.DB) ([]Loader, error) {
	rows, err := db.Query(`SELECT ` + loaderCols + ` FROM bin_loaders WHERE archived_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list loaders: %w", err)
	}
	defer rows.Close()
	var out []Loader
	for rows.Next() {
		l, err := scanLoader(rows)
		if err != nil {
			return nil, fmt.Errorf("scan loader: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateLoader updates the editable fields and bumps config_gen. The surrogate id and
// role are the fixed identity and are not updated here.
func UpdateLoader(db *sql.DB, l Loader) error {
	res, err := db.Exec(`
		UPDATE bin_loaders SET name=$1, layout=$2, replenishment=$3,
			outbound_dest=$4, inbound_source=$5, funnel_windows=$6,
			config_gen=config_gen+1, updated_at=NOW()
		WHERE id=$7`,
		l.Name, l.Layout, l.Replenishment, l.OutboundDest, l.InboundSource, l.FunnelWindows, l.ID)
	if err != nil {
		return fmt.Errorf("update loader %d: %w", l.ID, err)
	}
	return requireOne(res, "update loader", l.ID)
}

// DeleteLoader SOFT-deletes a loader: it sets archived_at instead of removing the row,
// so the stamped bin_uop_ledger history (loader_id is non-cascading) survives a retired
// loader — the whole reason the cascade was removed (6 reviewers flagged it). The
// homes/payloads rows are left intact (a hard DELETE would have cascaded them away).
// Active reads (ListLoaders) filter on archived_at IS NULL, so an archived loader stops
// syncing to Edge and driving demand; analytics read bin_uop_ledger, which is preserved.
// Idempotent: re-archiving just re-stamps archived_at. config_gen bumps so the next
// downward sync drops the loader from the Edge cache.
//
// A soft-retire + recreate now lands cleanly: identity is the surrogate id, so a
// replacement loader gets a fresh id and never collides with the archived one (the
// old UNIQUE(core_node_name, role) that used to block this is gone).
func DeleteLoader(db *sql.DB, id int64) error {
	res, err := db.Exec(`UPDATE bin_loaders SET archived_at=NOW(), config_gen=config_gen+1, updated_at=NOW() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("archive loader %d: %w", id, err)
	}
	return requireOne(res, "archive loader", id)
}

// UpsertHome assigns (or replaces) the payload at a dedicated position. The
// global UNIQUE(position_node_id) means a position belongs to exactly one loader
// and carries one payload; ON CONFLICT moves/relabels it. Bumps config_gen.
//
// Same payload on a second position is allowed (D1) — there is deliberately no
// UNIQUE(loader_id, payload_code) — two homes for a high-runner is legitimate.
func UpsertHome(db *sql.DB, h Home) error {
	// A blank kind normalises to HOME: every legacy/zero-value caller creates a home
	// position, and a BUFFER slot is written with an explicit kind. Pinning it here
	// keeps the NOT NULL/CHECK column satisfied without touching every caller.
	kind := h.Kind
	if kind == "" {
		kind = HomeKindHome
	}
	// sort_order is set on INSERT (append position) but deliberately NOT in the
	// ON CONFLICT SET — re-assigning a position's payload must preserve its place
	// in the order; only SetHomeOrder rewrites it. home_kind IS in the SET so
	// dragging a member between the Positions and Buffer zones re-kinds it.
	// min_stock is dormant (bin-count floor retired); leave the column at its
	// default rather than writing it.
	_, err := db.Exec(`
		INSERT INTO bin_loader_homes (loader_id, position_node_id, payload_code, home_kind, uop_threshold, sort_order)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (position_node_id) DO UPDATE SET
			loader_id=EXCLUDED.loader_id, payload_code=EXCLUDED.payload_code,
			home_kind=EXCLUDED.home_kind, uop_threshold=EXCLUDED.uop_threshold`,
		h.LoaderID, h.PositionNodeID, h.PayloadCode, kind, h.UOPThreshold, h.SortOrder)
	if err != nil {
		return fmt.Errorf("upsert home pos=%d: %w", h.PositionNodeID, err)
	}
	return bumpGen(db, h.LoaderID)
}

// SetHomeOrder rewrites sort_order for a loader's positions to match the given
// node-id sequence (index = order). Positions not in the list are left
// untouched. Bumps config_gen. Used by the grid-drag reorder.
func SetHomeOrder(db *sql.DB, loaderID int64, orderedNodeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("reorder homes loader=%d: begin: %w", loaderID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for i, nodeID := range orderedNodeIDs {
		if _, err := tx.Exec(`UPDATE bin_loader_homes SET sort_order=$1 WHERE loader_id=$2 AND position_node_id=$3`,
			i, loaderID, nodeID); err != nil {
			return fmt.Errorf("reorder home pos=%d: %w", nodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reorder homes loader=%d: commit: %w", loaderID, err)
	}
	return bumpGen(db, loaderID)
}

// RemoveHome clears a position assignment and bumps config_gen.
func RemoveHome(db *sql.DB, loaderID, positionNodeID int64) error {
	if _, err := db.Exec(`DELETE FROM bin_loader_homes WHERE loader_id=$1 AND position_node_id=$2`, loaderID, positionNodeID); err != nil {
		return fmt.Errorf("remove home pos=%d: %w", positionNodeID, err)
	}
	return bumpGen(db, loaderID)
}

// ListHomes returns a loader's positions in operator-defined order (sort_order,
// then position node id as a stable tiebreak).
func ListHomes(db *sql.DB, loaderID int64) ([]Home, error) {
	rows, err := db.Query(`SELECT loader_id, position_node_id, payload_code, home_kind, uop_threshold, sort_order
		FROM bin_loader_homes WHERE loader_id=$1 ORDER BY sort_order, position_node_id`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("list homes loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	var out []Home
	for rows.Next() {
		var h Home
		if err := rows.Scan(&h.LoaderID, &h.PositionNodeID, &h.PayloadCode, &h.Kind, &h.UOPThreshold, &h.SortOrder); err != nil {
			return nil, fmt.Errorf("scan home: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetHomeByPositionNode returns the dedicated-position home row for a node, or
// (nil, nil) if the node is not a loader position. The global
// UNIQUE(position_node_id) guarantees at most one match, so this resolves a
// physical node to its owning loader + the part pinned there — the Core-side
// counterpart to the Edge's LoaderForNode.
func GetHomeByPositionNode(db *sql.DB, positionNodeID int64) (*Home, error) {
	var h Home
	err := db.QueryRow(`SELECT loader_id, position_node_id, payload_code, home_kind, uop_threshold, sort_order
		FROM bin_loader_homes WHERE position_node_id=$1`, positionNodeID).
		Scan(&h.LoaderID, &h.PositionNodeID, &h.PayloadCode, &h.Kind, &h.UOPThreshold, &h.SortOrder)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get home by position node %d: %w", positionNodeID, err)
	}
	return &h, nil
}

// UpsertPayload adds or updates an allowed payload on a shared_window loader.
// Bumps config_gen.
func UpsertPayload(db *sql.DB, p Payload) error {
	_, err := db.Exec(`
		INSERT INTO bin_loader_payloads (loader_id, payload_code, uop_threshold)
		VALUES ($1,$2,$3)
		ON CONFLICT (loader_id, payload_code) DO UPDATE SET
			uop_threshold=EXCLUDED.uop_threshold`,
		p.LoaderID, p.PayloadCode, p.UOPThreshold)
	if err != nil {
		return fmt.Errorf("upsert payload %s/loader=%d: %w", p.PayloadCode, p.LoaderID, err)
	}
	return bumpGen(db, p.LoaderID)
}

// RemovePayload drops an allowed payload and bumps config_gen.
func RemovePayload(db *sql.DB, loaderID int64, payloadCode string) error {
	if _, err := db.Exec(`DELETE FROM bin_loader_payloads WHERE loader_id=$1 AND payload_code=$2`, loaderID, payloadCode); err != nil {
		return fmt.Errorf("remove payload %s/loader=%d: %w", payloadCode, loaderID, err)
	}
	return bumpGen(db, loaderID)
}

// ListPayloads returns a loader's allowed payload set, ordered by code.
func ListPayloads(db *sql.DB, loaderID int64) ([]Payload, error) {
	rows, err := db.Query(`SELECT loader_id, payload_code, uop_threshold
		FROM bin_loader_payloads WHERE loader_id=$1 ORDER BY payload_code`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("list payloads loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	var out []Payload
	for rows.Next() {
		var p Payload
		if err := rows.Scan(&p.LoaderID, &p.PayloadCode, &p.UOPThreshold); err != nil {
			return nil, fmt.Errorf("scan payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Quota is one line of a loader's declared carrier mix: how many carriers of a
// bin type it wants on hand.
//
// INTENT, and a PREFERENCE rather than a cap. never-2N still bounds how many
// carriers exist at a loader — in flight plus resident must not exceed the
// window count — and the quota only decides WHICH type to fetch next inside
// that bound. Made a cap, this would move the counting into the seam the
// 2026-07-31 over-ordering incident was about; as a preference the seam counts
// exactly as it does today.
//
// A total below the window count is legitimate and had no expression before:
// "four carriers at a five-window loader" is a thing an operator can now say. A
// total above it is bounded by the windows and never over-fetches.
type Quota struct {
	LoaderID    int64  `json:"loader_id"`
	BinTypeID   int64  `json:"bin_type_id"`
	BinTypeCode string `json:"bin_type_code"`
	Want        int    `json:"want"`
}

// UpsertQuota sets how many carriers of one bin type a loader wants, and bumps
// config_gen so the plant re-syncs. want=0 is a legitimate value meaning "none
// of this type"; RemoveQuota is how a line is dropped entirely.
func UpsertQuota(db *sql.DB, q Quota) error {
	_, err := db.Exec(`
		INSERT INTO bin_loader_quotas (loader_id, bin_type_id, want)
		VALUES ($1,$2,$3)
		ON CONFLICT (loader_id, bin_type_id) DO UPDATE SET want=EXCLUDED.want`,
		q.LoaderID, q.BinTypeID, q.Want)
	if err != nil {
		return fmt.Errorf("upsert quota bin_type=%d/loader=%d: %w", q.BinTypeID, q.LoaderID, err)
	}
	return bumpGen(db, q.LoaderID)
}

// RemoveQuota drops one line of the mix and bumps config_gen.
func RemoveQuota(db *sql.DB, loaderID, binTypeID int64) error {
	if _, err := db.Exec(`DELETE FROM bin_loader_quotas WHERE loader_id=$1 AND bin_type_id=$2`, loaderID, binTypeID); err != nil {
		return fmt.Errorf("remove quota bin_type=%d/loader=%d: %w", binTypeID, loaderID, err)
	}
	return bumpGen(db, loaderID)
}

// ListQuotas returns a loader's declared carrier mix with the bin-type CODES
// joined on, because the code is what travels on the wire and what a person
// reads — the id is a local key.
func ListQuotas(db *sql.DB, loaderID int64) ([]Quota, error) {
	rows, err := db.Query(`SELECT q.loader_id, q.bin_type_id, bt.code, q.want
		FROM bin_loader_quotas q JOIN bin_types bt ON bt.id = q.bin_type_id
		WHERE q.loader_id=$1 ORDER BY bt.code`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("list quotas loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	var out []Quota
	for rows.Next() {
		var q Quota
		if err := rows.Scan(&q.LoaderID, &q.BinTypeID, &q.BinTypeCode, &q.Want); err != nil {
			return nil, fmt.Errorf("scan quota: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SetHomeBinTypes replaces what a window can PHYSICALLY take with the given set
// of bin-type ids. An EMPTY set means the window takes anything, which is what
// every window does today and what every window keeps doing until somebody says
// otherwise.
//
// Physical, and therefore per window rather than per loader: a slot either fits
// a carrier or it does not, and that is a fact about the floor. When the floor
// is rebuilt somebody edits this; Shingo does not model why.
//
// Keyed on the position node alone — bin_loader_homes is UNIQUE on it, one
// loader per member node, so the node identifies the window by itself.
func SetHomeBinTypes(db *sql.DB, loaderID, positionNodeID int64, binTypeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin set home bin types: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM bin_loader_home_bin_types WHERE position_node_id=$1`, positionNodeID); err != nil {
		return fmt.Errorf("clear home bin types node=%d: %w", positionNodeID, err)
	}
	for _, id := range binTypeIDs {
		if _, err := tx.Exec(`INSERT INTO bin_loader_home_bin_types (position_node_id, bin_type_id) VALUES ($1,$2)`,
			positionNodeID, id); err != nil {
			return fmt.Errorf("add home bin type %d node=%d: %w", id, positionNodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set home bin types node=%d: %w", positionNodeID, err)
	}
	return bumpGen(db, loaderID)
}

// ListHomeBinTypes returns each window's capability set as bin-type CODES,
// keyed by position node id. A window absent from the map takes anything.
func ListHomeBinTypes(db *sql.DB, loaderID int64) (map[int64][]string, error) {
	rows, err := db.Query(`SELECT t.position_node_id, bt.code
		FROM bin_loader_home_bin_types t
		JOIN bin_types bt ON bt.id = t.bin_type_id
		JOIN bin_loader_homes h ON h.position_node_id = t.position_node_id
		WHERE h.loader_id=$1
		ORDER BY t.position_node_id, bt.code`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("list home bin types loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var nodeID int64
		var code string
		if err := rows.Scan(&nodeID, &code); err != nil {
			return nil, fmt.Errorf("scan home bin type: %w", err)
		}
		out[nodeID] = append(out[nodeID], code)
	}
	return out, rows.Err()
}

// GetConfig assembles a loader with its homes and payloads, or (nil, nil) if the
// loader is absent.
func GetConfig(db *sql.DB, id int64) (*Config, error) {
	l, err := GetLoader(db, id)
	if err != nil || l == nil {
		return nil, err
	}
	homes, err := ListHomes(db, id)
	if err != nil {
		return nil, err
	}
	payloads, err := ListPayloads(db, id)
	if err != nil {
		return nil, err
	}
	return &Config{Loader: *l, Homes: homes, Payloads: payloads}, nil
}

// MemberNodeNames maps a loader's member position node ids to their node names,
// in one query.
//
// The alternative — walking a loader's homes and resolving each id separately —
// is what the downward config sync does, and it costs a query per window. That
// is tolerable on a sync that runs when config changes and unreasonable on a
// decision that runs every time a loop drops below its threshold.
//
// A home whose node has vanished is simply absent from the map, matching the
// sync's disposition: skip the member rather than fail the whole answer.
func MemberNodeNames(db *sql.DB, loaderID int64) (map[int64]string, error) {
	rows, err := db.Query(`
		SELECT h.position_node_id, n.name
		FROM bin_loader_homes h JOIN nodes n ON n.id = h.position_node_id
		WHERE h.loader_id = $1`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("member node names loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan member node name: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

func bumpGen(db *sql.DB, loaderID int64) error {
	if _, err := db.Exec(`UPDATE bin_loaders SET config_gen=config_gen+1, updated_at=NOW() WHERE id=$1`, loaderID); err != nil {
		return fmt.Errorf("bump config_gen loader %d: %w", loaderID, err)
	}
	return nil
}

func requireOne(res sql.Result, op string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %d: rows affected: %w", op, id, err)
	}
	if n == 0 {
		return fmt.Errorf("%s %d: no such row", op, id)
	}
	return nil
}
