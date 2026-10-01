// loader_two_stage.go — a two-stage unloader set up as ONE unloader.
//
// Stage 1 pulls the full bin off and leaves the cart bare; stage 2 sets a new
// empty bin on it and sends it out. Underneath they are two consume loaders, and
// they stay two: each runs its own windows on the Edge exactly as the half-loader
// shipped. What this file adds is the setup and the plumbing between them:
//
//   - the link: stage 1's second_stage_loader_id, so the page shows one box.
//   - bare is a property of the cart type (bin_types.bare_of), not of the
//     loader: stage 1's CLEAR stamps each cart's own marker, derived from its
//     type the first time it comes through (bins.EnsureBareMarkerTx). Several
//     cart types through one pair is the default and needs no configuration.
//   - where stage 1 sends a cart is DERIVED, never typed. Pull mode: stage 2
//     has an inbound source (the wait group), and stage 1 sends there;
//     engine/stage2_pull.go moves each cart on into a free stage-2 window.
//     Direct mode: stage 2 has none, and stage 1 sends to stage 2's one window,
//     or to a plain group Core makes for several and keeps in step.
//   - stage 1's windows stand in a group Core makes too, so a press or cell
//     feeding stage 1 has one name to pick as its outbound destination. It is
//     the same problem as stage 2's group in the other direction.
//   - "pulled directly by the process" on stage 2: its finished carts stay on
//     its windows, and the windows stand in a group Core makes at the first
//     one, in both modes, so a line has one name to source its empties from.

package service

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// BareMarkerSuffix names the bare marker derived from a carrier type.
const BareMarkerSuffix = bins.BareMarkerSuffix

// ErrBareMarkerTaken refuses a derived marker code that already names a type
// that is not bare, which would make stage 1 stamp an ordinary empty.
var ErrBareMarkerTaken = bins.ErrBareMarkerTaken

// ErrWindowInAnotherGroup refuses a window Core would have to take out of the
// group it already stands in: a stage-1 window, or a stage-2 window of a direct
// pair. Core parents those into a group of its own, and a node has one parent:
// taking it would silently pull it out of the group it is in.
var ErrWindowInAnotherGroup = errors.New("this node is already in another group: take it out of that group first")

// ErrPulledDirectlyStage2Only refuses "pulled directly by the process" on
// anything but a two-stage unloader's stage 2: it is about the carts stage 2
// finishes, and only stage 2 finishes carts.
var ErrPulledDirectlyStage2Only = errors.New("pulled directly by the process is a two-stage unloader's stage 2 setting")

// ErrQuotaBare refuses a bare marker in a loader's carrier mix: the mix says
// which empties to fetch, and no finder hands out a bare cart.
var ErrQuotaBare = errors.New("a bare cart type cannot be in a loader's carrier mix: no empty finder hands out a bare cart")

// stage1Suffix and stage2Suffix name the two halves after the pair.
const (
	stage1Suffix = " · stage 1"
	stage2Suffix = " · stage 2"
)

// TwoStageCreate is CreateTwoStage's argument. Nothing about placement: the
// windows, where fulls come from and where carts go are all set on the box
// afterwards, through the ordinary loader update and add-home doors.
type TwoStageCreate struct {
	Name           string
	AcceptPartials bool
	AutoPush       bool
}

// CreateTwoStage creates both stages and the link. Returns the stage-1 and
// stage-2 loader ids.
func (s *LoaderService) CreateTwoStage(in TwoStageCreate) (stage1, stage2 int64, err error) {
	if in.Name == "" {
		return 0, 0, errors.New("name is required")
	}
	stage2, err = s.db.CreateLoader(loaders.Loader{
		Name: in.Name + stage2Suffix, Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
		Replenishment: loaders.ReplenishmentOperator,
	})
	if err != nil {
		return 0, 0, err
	}
	stage1, err = s.db.CreateLoader(loaders.Loader{
		Name: in.Name + stage1Suffix, Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
		Replenishment:  loaders.ReplenishmentOperator,
		AcceptPartials: in.AcceptPartials, AutoPush: in.AutoPush, SecondStageLoaderID: &stage2,
	})
	if err != nil {
		if derr := s.db.DeleteLoader(stage2); derr != nil {
			log.Printf("loader_service: two-stage %q: stage 1 failed and stage 2 (%d) could not be archived: %v", in.Name, stage2, derr)
		}
		return 0, 0, err
	}
	s.rederive()
	return stage1, stage2, nil
}

// firstStageOf returns the stage-1 loader whose stage 2 is loaderID, or nil.
func (s *LoaderService) firstStageOf(loaderID int64) (*loaders.Loader, error) {
	all, err := s.db.ListLoaders()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if l := all[i]; l.SecondStageLoaderID != nil && *l.SecondStageLoaderID == loaderID {
			return &all[i], nil
		}
	}
	return nil, nil
}

// pairOf returns the pair loaderID belongs to — its stage 1 and stage 2 — or
// nils for a loader that is not half of one.
func (s *LoaderService) pairOf(loaderID int64) (*loaders.Loader, *loaders.Loader, error) {
	cur, err := s.db.GetLoader(loaderID)
	if err != nil || cur == nil {
		return nil, nil, err
	}
	one := cur
	if cur.SecondStageLoaderID == nil {
		if one, err = s.firstStageOf(loaderID); err != nil || one == nil {
			return nil, nil, err
		}
	}
	two, err := s.db.GetLoader(*one.SecondStageLoaderID)
	if err != nil || two == nil {
		return nil, nil, err
	}
	return one, two, nil
}

// stageGroupName is the name of the group Core makes for one stage's windows:
// the pair's name and the stage, so it reads as what it is on the nodes page.
func stageGroupName(one *loaders.Loader, suffix string) string {
	return strings.TrimSuffix(one.Name, stage1Suffix) + suffix
}

// pairGroupName names the group Core makes for a direct pair's stage-2 windows.
func pairGroupName(one *loaders.Loader) string { return stageGroupName(one, stage2Suffix) }

// stage1GroupName names the group Core makes for stage 1's windows.
func stage1GroupName(one *loaders.Loader) string { return stageGroupName(one, stage1Suffix) }

// madeGroup returns the group Core made under name, or nil. It is never the
// stage's inbound source, a group the plant made, even if somebody named one
// the same.
func (s *LoaderService) madeGroup(name, inboundSource string) *nodes.Node {
	if name == inboundSource {
		return nil
	}
	g, err := s.db.GetNodeByDotName(name)
	if err != nil || g == nil || !g.IsSynthetic {
		return nil
	}
	return g
}

// pairGroup returns the group Core made for a direct pair's stage-2 windows, or
// nil. It is never the plant's own wait group.
func (s *LoaderService) pairGroup(one, two *loaders.Loader) *nodes.Node {
	return s.madeGroup(pairGroupName(one), two.InboundSource)
}

// stage1Group returns the group Core made for stage 1's windows, or nil.
func (s *LoaderService) stage1Group(one *loaders.Loader) *nodes.Node {
	return s.madeGroup(stage1GroupName(one), one.InboundSource)
}

// carriedGroup is a group Core made for the pair, and which stage it is for,
// read before a rename so the rename can carry it.
type carriedGroup struct {
	node   *nodes.Node
	suffix string
}

// pairGroupsBeforeRename returns the groups Core made for the pair whose stage 1
// is loaderID, when newName renames that stage 1 — read under the current name,
// before the rename is written. Empty when loaderID is not a stage 1, the name
// is unchanged, or the pair has no such group.
func (s *LoaderService) pairGroupsBeforeRename(loaderID int64, newName string) ([]carriedGroup, error) {
	one, two, err := s.pairOf(loaderID)
	if err != nil || one == nil || one.ID != loaderID || one.Name == newName {
		return nil, err
	}
	var out []carriedGroup
	if g := s.stage1Group(one); g != nil {
		out = append(out, carriedGroup{g, stage1Suffix})
	}
	if g := s.pairGroup(one, two); g != nil {
		out = append(out, carriedGroup{g, stage2Suffix})
	}
	return out, nil
}

// dropGroup deletes a group Core made for the pair, which reparents its windows
// back to the grid (nodes.DeleteGroup). nil is nothing to drop.
func (s *LoaderService) dropGroup(g *nodes.Node, one *loaders.Loader) error {
	if g == nil {
		return nil
	}
	if err := s.db.DeleteNodeGroup(g.ID); err != nil {
		return fmt.Errorf("drop the group %s made for %s: %w", g.Name, one.Name, err)
	}
	return nil
}

// dropPairGroup deletes the group Core made for a direct pair's stage-2 windows.
func (s *LoaderService) dropPairGroup(one, two *loaders.Loader) error {
	return s.dropGroup(s.pairGroup(one, two), one)
}

// checkPairWindow refuses a window Core would have to take out of another
// group: any stage-1 window, and a stage-2 window of a direct pair or of one
// pulled directly. Stage 2 in pull mode otherwise never parents a window, so
// it has nothing to refuse.
func (s *LoaderService) checkPairWindow(loaderID int64, node *nodes.Node) error {
	one, two, err := s.pairOf(loaderID)
	if err != nil || one == nil || node.ParentID == nil {
		return err
	}
	var own string
	switch {
	case one.ID == loaderID:
		own = stage1GroupName(one)
	case two.InboundSource == "" || two.PulledDirectly:
		own = pairGroupName(one)
	default:
		return nil
	}
	parent, err := s.db.GetNode(*node.ParentID)
	if err != nil {
		return fmt.Errorf("read the group %s stands in: %w", node.Name, err)
	}
	if parent.Name == own {
		return nil
	}
	return fmt.Errorf("%w (%s is in %s)", ErrWindowInAnotherGroup, node.Name, parent.Name)
}

// syncPair derives where stage 1 sends a cart, keeps a direct pair's group in
// step with stage 2's windows, and keeps stage 1's group in step with its own
// (syncStage1Group):
//
//   - pull mode (stage 2 has an inbound source): stage 1 sends to that group,
//     and a group Core made for direct mode is dropped.
//   - direct, no windows: nowhere (the CLEAR refuses, naming it).
//   - direct, one window: that node; a group Core made is dropped.
//   - direct, several: a plain group Core makes, holding every window.
//   - pulled directly (either mode): the group from the first window, as for
//     stage 1's, so a line names one group; a direct pair's stage 1 sends there.
func (s *LoaderService) syncPair(one, two *loaders.Loader) error {
	dest := two.InboundSource
	if two.PulledDirectly {
		var err error
		if dest, err = s.syncPulledDirectlyGroup(one, two); err != nil {
			return err
		}
	} else if dest != "" {
		if err := s.dropPairGroup(one, two); err != nil {
			return err
		}
	} else {
		homes, err := s.db.ListLoaderHomes(two.ID)
		if err != nil {
			return err
		}
		switch len(homes) {
		case 0, 1:
			if err := s.dropPairGroup(one, two); err != nil {
				return err
			}
			if len(homes) == 1 {
				n, err := s.db.GetNode(homes[0].PositionNodeID)
				if err != nil {
					return fmt.Errorf("stage-2 window %d: %w", homes[0].PositionNodeID, err)
				}
				dest = n.Name
			}
		default:
			if dest, err = s.groupStage2Windows(one, two, homes); err != nil {
				return err
			}
		}
	}
	// Stage 1's destination is written before its own group is synced, so a
	// stage-1 window standing in another group refuses the grouping without
	// holding the cart's destination hostage.
	if one.OutboundDest != dest {
		one.OutboundDest = dest
		if err := s.db.UpdateLoader(*one); err != nil {
			return err
		}
	}
	return s.syncStage1Group(one)
}

// syncPulledDirectlyGroup keeps a pulled-directly stage 2's group in step with
// its windows, made at the first one, and returns where stage 1 sends a cart:
// the wait group in pull mode (the stage-2 pull moves carts into the windows,
// as without the option), the group itself in direct mode, nowhere with no
// windows.
func (s *LoaderService) syncPulledDirectlyGroup(one, two *loaders.Loader) (string, error) {
	homes, err := s.db.ListLoaderHomes(two.ID)
	if err != nil {
		return "", err
	}
	if len(homes) == 0 {
		return two.InboundSource, s.dropPairGroup(one, two)
	}
	group, err := s.groupStage2Windows(one, two, homes)
	if err != nil || two.InboundSource != "" {
		return two.InboundSource, err
	}
	return group, nil
}

// syncStage1Group keeps the group Core makes for stage 1's windows in step with
// them. A press or cell feeding stage 1 names where its fulls go as ONE outbound
// destination, so stage 1's windows need a group to be named by, exactly as
// stage 2's windows need one for stage 1 to send its carts to.
//
// MADE AT THE FIRST WINDOW, NOT THE SECOND, which is where it differs from stage
// 2's. Core writes stage 1's destination itself, so stage 2's can switch between
// a node and a group as windows come and go. Stage 1's name is typed into a
// claim by hand, and adding a window must not change it under that claim.
//
// KEPT WHETHER STAGE 1 IS FED DIRECTLY OR PULLS: Core's own pulls into stage 1
// name the window, never the group, so the group changes nothing for them.
func (s *LoaderService) syncStage1Group(one *loaders.Loader) error {
	homes, err := s.db.ListLoaderHomes(one.ID)
	if err != nil {
		return err
	}
	if len(homes) == 0 {
		return s.dropGroup(s.stage1Group(one), one)
	}
	_, err = s.groupWindows(stage1GroupName(one), s.stage1Group(one), one, homes)
	return err
}

// groupStage2Windows parents every stage-2 window into the pair's group,
// creating it the first time, and returns its name.
func (s *LoaderService) groupStage2Windows(one, two *loaders.Loader, homes []loaders.Home) (string, error) {
	return s.groupWindows(pairGroupName(one), s.pairGroup(one, two), one, homes)
}

// groupWindows parents every window in homes into g, creating it under name the
// first time, and returns its name.
func (s *LoaderService) groupWindows(name string, g *nodes.Node, one *loaders.Loader, homes []loaders.Home) (string, error) {
	if g == nil {
		id, err := s.db.CreateNodeGroup(name)
		if err != nil {
			return "", fmt.Errorf("create the group for %s: %w", one.Name, err)
		}
		if g, err = s.db.GetNode(id); err != nil {
			return "", err
		}
	}
	for _, h := range homes {
		n, err := s.db.GetNode(h.PositionNodeID)
		if err != nil {
			return "", fmt.Errorf("window %d: %w", h.PositionNodeID, err)
		}
		if n.ParentID != nil && *n.ParentID == g.ID {
			continue
		}
		if n.ParentID != nil {
			parent, err := s.db.GetNode(*n.ParentID)
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("%w (%s is in %s)", ErrWindowInAnotherGroup, n.Name, parent.Name)
		}
		if err := s.db.ReparentNode(n.ID, &g.ID, 0); err != nil {
			return "", fmt.Errorf("put %s in %s: %w", n.Name, g.Name, err)
		}
	}
	return g.Name, nil
}

// SyncPairs re-derives every pair, so a pair set up before stage 1 had a group
// gets one at startup instead of at its next edit. A pair that cannot be synced
// is logged and left as it was; the rest still sync.
func (s *LoaderService) SyncPairs() {
	all, err := s.db.ListLoaders()
	if err != nil {
		log.Printf("loader_service: sync two-stage pairs: %v", err)
		return
	}
	for _, l := range all {
		if l.SecondStageLoaderID == nil || l.ArchivedAt != nil {
			continue
		}
		if err := s.syncPairOf(l.ID); err != nil {
			log.Printf("loader_service: sync two-stage %s: %v", l.Name, err)
		}
	}
}

// syncPairOf re-derives the pair loaderID belongs to, if any.
func (s *LoaderService) syncPairOf(loaderID int64) error {
	one, two, err := s.pairOf(loaderID)
	if err != nil || one == nil {
		return err
	}
	return s.syncPair(one, two)
}

// deletePair archives both stages and drops the groups Core made for the pair,
// which gives their windows back to the grid.
func (s *LoaderService) deletePair(one, two *loaders.Loader) error {
	if err := s.dropPairGroup(one, two); err != nil {
		return err
	}
	if err := s.dropGroup(s.stage1Group(one), one); err != nil {
		return err
	}
	if err := s.db.DeleteLoader(one.ID); err != nil {
		return err
	}
	if err := s.db.DeleteLoader(two.ID); err != nil {
		return fmt.Errorf("archive stage 2 of loader %d: %w", one.ID, err)
	}
	return nil
}

// StageOneAt reports whether nodeID is a window of a stage 1, and where that
// stage 1 sends a cleared cart ("" = nowhere yet). The CLEAR asks it before
// clearing: a stage-1 CLEAR stamps the cart's marker, and one with nowhere to
// send the cart is refused before anything is written.
func (s *LoaderService) StageOneAt(nodeID int64) (bool, string, error) {
	h, err := s.db.GetLoaderHomeByPositionNode(nodeID)
	if err != nil || h == nil {
		return false, "", err
	}
	l, err := s.db.GetLoader(h.LoaderID)
	if err != nil || l == nil || l.ArchivedAt != nil || l.SecondStageLoaderID == nil {
		return false, "", err
	}
	return true, l.OutboundDest, nil
}
