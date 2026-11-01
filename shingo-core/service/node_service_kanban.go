package service

import (
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/plantclaims"
)

// Kanban configuration, service side.
//
// A kanban is an NGRP whose EMPTY-CARRIER level Core holds: so many unclaimed
// carriers of each declared type, near the equipment that consumes them. This
// file is the read/write surface the group settings modal talks to.
//
// The level keeper (engine/kanban_keeper.go) holds the levels saved here. The
// layer wants saved beside them tell the unloader which heights the group is
// short of; the one at 0 layers (tops & bottoms) is a cap of its own, beside
// the type's stack level, and the keeper fetches neither.
//
// ONE WRITE. Everything is reached through SaveKanban: the rules, then one
// transaction that applies the change and appends one audit row per changed
// field or list entry (store.SaveKanban). The per-thing methods below each carry
// their partial to it.

// KanbanConfig is one group's whole configuration, as the settings modal
// reads it: the scalars, the levels with every level's wanted heights
// (LayerWants, in the levels' order), the supports and the sources, in one
// answer.
//
// One read rather than six, because a half-loaded configuration screen is a
// screen an operator can save from — and saving what a failed fetch left blank
// is how a level silently becomes zero.
type KanbanConfig struct {
	GroupNodeID         int64                   `json:"group_node_id"`
	StrictSourcing      bool                    `json:"strict_sourcing"`
	MaintenanceStation  string                  `json:"maintenance_station"`
	OverflowDestination string                  `json:"overflow_destination"`
	RunningCover        int                     `json:"running_cover"`
	Levels              []store.KanbanLevel     `json:"levels"`
	LayerWants          []store.KanbanLayerWant `json:"layer_wants"`
	Supports            []store.KanbanSupport   `json:"supports"`
	Sources             []store.KanbanSource    `json:"sources"`
	// hasRow says the group has its kanbans row. overflowNodeID is the default
	// overflow's node, 0 when none; a plain group's overflow is its property's name
	// alone, and may not resolve.
	hasRow         bool
	overflowNodeID int64
}

// isKanban says the configuration needs a kanbans row: it has one, or something
// in it hangs off one. A group that is none of these stays plain, and a save of
// its all-default settings (the modal posts them for every group) writes nothing.
func (c KanbanConfig) isKanban() bool {
	return c.hasRow || len(c.Levels) > 0 || len(c.Supports) > 0 || len(c.Sources) > 0 ||
		len(c.LayerWants) > 0 || c.StrictSourcing || c.RunningCover > 0 ||
		strings.TrimSpace(c.MaintenanceStation) != ""
}

// GetKanban returns everything the modal needs for one group.
//
// A group with nothing configured is not an error and not a nil: it is a config
// with empty sets, which is what every plain NGRP looks like and what the modal
// must be able to render. A kanban's overflow comes off its row, a plain group's
// off its property (store.OverflowTargetOf reads whichever applies).
func (s *NodeService) GetKanban(groupNodeID int64) (*KanbanConfig, error) {
	levels, err := s.db.ListDeclaredLevels(groupNodeID)
	if err != nil {
		return nil, err
	}
	layerWants, err := s.db.ListKanbanLayerWants(groupNodeID)
	if err != nil {
		return nil, err
	}
	supports, err := s.db.ListKanbanSupports(groupNodeID)
	if err != nil {
		return nil, err
	}
	sources, err := s.db.ListKanbanSources(groupNodeID)
	if err != nil {
		return nil, err
	}
	k, err := s.db.GetKanban(groupNodeID)
	if err != nil {
		return nil, err
	}
	overflow, err := s.db.OverflowTargetOf(groupNodeID)
	if err != nil {
		return nil, err
	}
	cfg := &KanbanConfig{
		GroupNodeID:         groupNodeID,
		OverflowDestination: overflow.Name,
		overflowNodeID:      overflow.NodeID,
		Levels:              levels,
		LayerWants:          layerWants,
		Supports:            supports,
		Sources:             sources,
	}
	if k != nil {
		cfg.hasRow = true
		cfg.StrictSourcing = k.Strict
		cfg.MaintenanceStation = k.StationID
		cfg.RunningCover = k.RunningCover
	}
	return cfg, nil
}

// KanbanSettings is the settings cluster as one save: strict, station
// and default overflow.
type KanbanSettings struct {
	StrictSourcing      bool
	MaintenanceStation  string
	OverflowDestination string
}

// KanbanSave is one save of a kanban's configuration. A nil field leaves what is
// stored, so an endpoint carries only the thing an operator edited.
type KanbanSave struct {
	GroupNodeID int64
	Actor       string
	Strict      *bool
	Station     *string
	// Overflow names the default overflow by name; blank clears it. On a kanban it
	// must resolve to another node; a plain group keeps the name as written.
	Overflow *string
	Cover    *int
	// Levels are the level lines to set, RemoveLevels those to stop declaring.
	Levels       []KanbanLevelSave
	RemoveLevels []int64
	Supports     *[]int64
	Sources      *[]int64
}

// KanbanLevelSave is one level line of a save. A line with no Want changes only
// what it names, on a level that exists.
type KanbanLevelSave struct {
	BinTypeID  int64
	Want       *int
	LayerWants *[]KanbanLayerWant
	// Overflow is the type's own overflow node id; 0 clears it, and the type takes
	// the kanban's default.
	Overflow *int64
}

// KanbanLayerWant is one wanted height of a level (store.KanbanLayerWant),
// named here so the www layer can build a save without the store.
type KanbanLayerWant = store.KanbanLayerWant

// SaveKanban is the one write of a kanban's configuration: it builds the
// configuration the save would leave behind, asks the rules, and hands what
// passed to store.SaveKanban. A refusal writes nothing and is the returned check,
// not the error, which is for a read or write that failed.
func (s *NodeService) SaveKanban(save KanbanSave) (KanbanCheck, error) {
	post, err := s.GetKanban(save.GroupNodeID)
	if err != nil {
		return KanbanCheck{}, err
	}
	refusals, err := s.applyKanbanSave(post, save)
	if err != nil {
		return KanbanCheck{}, err
	}
	if len(refusals) > 0 {
		return KanbanCheck{Refusals: refusals}, nil
	}
	chk, err := s.ValidateKanban(*post)
	if err != nil || chk.Err() != nil {
		return chk, err
	}
	change, refusal, err := s.kanbanChange(post, save)
	if err != nil {
		return chk, err
	}
	if refusal != "" {
		chk.Refusals = append(chk.Refusals, refusal)
		return chk, nil
	}
	actor := save.Actor
	if actor == "" {
		actor = protocol.AuditActorUI
	}
	if err := s.db.SaveKanban(change, actor); err != nil {
		return chk, err
	}
	if save.changesTheLine() {
		s.notifyLevelChanged(save.GroupNodeID)
	}
	return chk, nil
}

// changesTheLine says the save changes what an Edge shown the group's line sees:
// a level or its heights, the cover, or the supports the cover counts over. The
// snapshot carries neither a kanban's sources nor its overflow, so those do not
// ring. A ring is latency: a save it misses reaches the Edge on the heartbeat's
// compare. Pin: TestKanbanSaves_RingTheLineOnlyWhenTheLineMoves.
func (s KanbanSave) changesTheLine() bool {
	if len(s.RemoveLevels) > 0 || s.Cover != nil || s.Supports != nil {
		return true
	}
	for _, lv := range s.Levels {
		if lv.Want != nil || lv.LayerWants != nil {
			return true
		}
	}
	return false
}

// applyKanbanSave turns post into the configuration save would leave behind, and
// returns what refuses a line on its own, before the whole is judged.
func (s *NodeService) applyKanbanSave(post *KanbanConfig, save KanbanSave) ([]string, error) {
	var refusals []string
	if save.Strict != nil {
		post.StrictSourcing = *save.Strict
	}
	if save.Station != nil {
		post.MaintenanceStation = *save.Station
	}
	if save.Overflow != nil {
		post.OverflowDestination = *save.Overflow
	}
	if save.Cover != nil {
		if *save.Cover < 0 {
			refusals = append(refusals, fmt.Sprintf("a running cover cannot be negative (got %d)", *save.Cover))
		}
		post.RunningCover = *save.Cover
	}
	if len(save.RemoveLevels) > 0 {
		kept := post.Levels[:0]
		for _, l := range post.Levels {
			if !containsID(save.RemoveLevels, l.BinTypeID) {
				kept = append(kept, l)
			}
		}
		post.Levels = kept
		keptWants := post.LayerWants[:0]
		for _, w := range post.LayerWants {
			if !containsID(save.RemoveLevels, w.BinTypeID) {
				keptWants = append(keptWants, w)
			}
		}
		post.LayerWants = keptWants
	}
	for _, lv := range save.Levels {
		msg, err := s.applyLevelSave(post, lv)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			refusals = append(refusals, msg)
		}
	}
	if save.Supports != nil {
		post.Supports = post.Supports[:0]
		for _, id := range *save.Supports {
			n, err := s.db.GetNode(id)
			if err != nil {
				return nil, fmt.Errorf("read supported position %d: %w", id, err)
			}
			post.Supports = append(post.Supports, store.KanbanSupport{
				GroupNodeID: post.GroupNodeID, ProcessNodeID: id, ProcessNodeName: n.Name,
			})
		}
	}
	if save.Sources != nil {
		post.Sources = post.Sources[:0]
		for i, id := range *save.Sources {
			n, err := s.db.GetNode(id)
			if err != nil {
				return nil, fmt.Errorf("read source %d: %w", id, err)
			}
			post.Sources = append(post.Sources, store.KanbanSource{
				KanbanNodeID: post.GroupNodeID, Rank: i + 1, SourceNodeID: id, SourceNodeName: n.Name,
			})
		}
	}
	return refusals, nil
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// applyLevelSave applies one level line to post, and returns what refuses it.
func (s *NodeService) applyLevelSave(post *KanbanConfig, lv KanbanLevelSave) (string, error) {
	if lv.Want != nil && *lv.Want < 0 {
		return fmt.Sprintf("a kanban level cannot be negative (got %d)", *lv.Want), nil
	}
	bt, err := s.db.GetBinType(lv.BinTypeID)
	if err != nil {
		return "", fmt.Errorf("read carrier type %d: %w", lv.BinTypeID, err)
	}
	// A level is N EMPTIES kept on hand, and a bare cart is nobody's empty
	// (bins.EmptyCarrierWhere): the keeper would chase a level it can never
	// count.
	if bt.Bare {
		return fmt.Sprintf("%s is a bare cart type: a group keeps empties, and no finder hands out a bare cart", bt.Code), nil
	}
	// A HEIGHT IS COUNTED IN TOTES, so a type that states none cannot be wanted
	// at one: no cart of it could ever be counted standing there.
	if lv.LayerWants != nil && len(*lv.LayerWants) > 0 && !protocol.IsToteType(bt.TotesPerLayer) {
		return fmt.Sprintf("%s states no totes per layer: a height, tops & bottoms included, is counted in its totes, so it cannot be wanted at one", bt.Code), nil
	}
	if lv.LayerWants != nil {
		kept := post.LayerWants[:0]
		for _, w := range post.LayerWants {
			if w.BinTypeID != lv.BinTypeID {
				kept = append(kept, w)
			}
		}
		for _, w := range *lv.LayerWants {
			w.GroupNodeID, w.BinTypeID, w.BinTypeCode = post.GroupNodeID, lv.BinTypeID, bt.Code
			kept = append(kept, w)
		}
		post.LayerWants = kept
	}
	var line *store.KanbanLevel
	for i := range post.Levels {
		if post.Levels[i].BinTypeID == lv.BinTypeID {
			line = &post.Levels[i]
		}
	}
	if line == nil {
		if lv.Want == nil {
			return fmt.Sprintf("%s has no level here to change", bt.Code), nil
		}
		post.Levels = append(post.Levels, store.KanbanLevel{
			GroupNodeID: post.GroupNodeID, BinTypeID: lv.BinTypeID, BinTypeCode: bt.Code, Want: *lv.Want,
		})
		return "", nil
	}
	if lv.Want != nil {
		line.Want = *lv.Want
	}
	return "", nil
}

// kanbanChange is the store's half of the save, and the refusal of an overflow a
// row cannot hold: a kanban's overflow is a node, and it is not the kanban.
func (s *NodeService) kanbanChange(post *KanbanConfig, save KanbanSave) (store.KanbanChange, string, error) {
	change := store.KanbanChange{
		NodeID:       save.GroupNodeID,
		EnsureRow:    post.isKanban(),
		Station:      save.Station,
		Strict:       save.Strict,
		Cover:        save.Cover,
		RemoveLevels: save.RemoveLevels,
		Supports:     save.Supports,
		Sources:      save.Sources,
	}
	for _, lv := range save.Levels {
		if lv.Overflow != nil && *lv.Overflow == save.GroupNodeID {
			return change, "a carrier type cannot overflow into the kanban that holds it", nil
		}
		if lv.Overflow != nil && *lv.Overflow != 0 {
			if _, err := s.db.GetNode(*lv.Overflow); err != nil {
				return change, fmt.Sprintf("overflow node %d does not exist", *lv.Overflow), nil
			}
		}
		change.Levels = append(change.Levels, store.KanbanLevelChange{
			BinTypeID: lv.BinTypeID, Want: lv.Want, LayerWants: lv.LayerWants, Overflow: lv.Overflow,
		})
	}
	if save.Overflow == nil {
		return change, "", nil
	}
	name := strings.TrimSpace(*save.Overflow)
	if !post.isKanban() {
		change.PlainOverflow = &name
		return change, "", nil
	}
	id := int64(0)
	if name != "" {
		n, err := s.db.GetNodeByDotName(name)
		if err != nil || n == nil {
			return change, fmt.Sprintf("overflow %q does not resolve to a node", name), nil
		}
		if n.ID == save.GroupNodeID {
			return change, fmt.Sprintf("%s cannot overflow into itself", name), nil
		}
		id = n.ID
	}
	change.Overflow = &id
	return change, "", nil
}

// SetKanbanLevel declares how many empty carriers of one type a group holds.
func (s *NodeService) SetKanbanLevel(groupNodeID, binTypeID int64, want int) (KanbanCheck, error) {
	return s.SaveKanban(KanbanSave{GroupNodeID: groupNodeID,
		Levels: []KanbanLevelSave{{BinTypeID: binTypeID, Want: &want}}})
}

// SetKanbanLevelAndLayerWants is SetKanbanLevel with the type's layer
// wants replaced by layerWants in the same transaction (an empty set clears
// them). A height on a type that states no totes per layer is refused, and the
// tops & bottoms row (0 layers) counts against the group's positions beside the
// level.
func (s *NodeService) SetKanbanLevelAndLayerWants(groupNodeID, binTypeID int64, want int,
	layerWants []KanbanLayerWant) (KanbanCheck, error) {
	if layerWants == nil {
		layerWants = []KanbanLayerWant{}
	}
	return s.SaveKanban(KanbanSave{GroupNodeID: groupNodeID,
		Levels: []KanbanLevelSave{{BinTypeID: binTypeID, Want: &want, LayerWants: &layerWants}}})
}

// RemoveKanbanLevel stops declaring a carrier type for a group, and its layer
// wants go with it. Distinct from setting want=0, which keeps the line.
func (s *NodeService) RemoveKanbanLevel(groupNodeID, binTypeID int64) (KanbanCheck, error) {
	return s.SaveKanban(KanbanSave{GroupNodeID: groupNodeID, RemoveLevels: []int64{binTypeID}})
}

// ListKanbanLevels returns a group's declared level.
func (s *NodeService) ListKanbanLevels(groupNodeID int64) ([]store.KanbanLevel, error) {
	return s.db.ListDeclaredLevels(groupNodeID)
}

// SetKanbanSupports replaces the set of process nodes a group serves.
func (s *NodeService) SetKanbanSupports(groupNodeID int64, processNodeIDs []int64) (KanbanCheck, error) {
	return s.SaveKanban(KanbanSave{GroupNodeID: groupNodeID, Supports: &processNodeIDs})
}

// CheckKanbanSettings runs the save-time rules against a settings change
// WITHOUT writing it. The settings endpoint runs this and the floor guard before
// it saves, so a refusal is named ahead of the question the guard asks.
func (s *NodeService) CheckKanbanSettings(groupNodeID int64, set KanbanSettings) (KanbanCheck, error) {
	post, err := s.GetKanban(groupNodeID)
	if err != nil {
		return KanbanCheck{}, err
	}
	post.StrictSourcing = set.StrictSourcing
	post.MaintenanceStation = set.MaintenanceStation
	post.OverflowDestination = set.OverflowDestination
	return s.ValidateKanban(*post)
}

// ListKanbanSupports returns the process nodes a group serves.
func (s *NodeService) ListKanbanSupports(groupNodeID int64) ([]store.KanbanSupport, error) {
	return s.db.ListKanbanSupports(groupNodeID)
}

// ListProcessNodeOptions returns each process with the Core nodes its claims
// resolve to — the picker's contents.
//
// The editor OFFERS processes and STORES nodes, and the resolution happens here,
// once, at config time. It has to: a claim lives on the Edge, so a rule stored as
// "process P" would have nothing Core could evaluate it against later. What the
// operator sees is a process; what lands in kanban_supports is the node
// set that process's claims name at the moment they save.
func (s *NodeService) ListProcessNodeOptions() ([]plantclaims.ProcessNodeOption, error) {
	return s.db.ListProcessNodeOptions()
}

// ProcessesByNode maps each Core node name to the processes whose setup names
// it, for the boards' Process column. See plantclaims.ProcessesByNode.
func (s *NodeService) ProcessesByNode() (map[string][]string, error) {
	return s.db.ProcessesByNode()
}
