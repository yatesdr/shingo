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

// ErrWindowInAnotherGroup refuses a stage-2 window of a direct pair that already
// stands in a group. Core parents a direct pair's windows into the group stage
// 1 sends to, and a node has one parent: taking it would silently pull it out of
// the group it is in.
var ErrWindowInAnotherGroup = errors.New("this node is already in another group: take it out of that group first")

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

// pairGroupName is the name of the group Core makes for a direct pair with
// several stage-2 windows: the pair's stage-2 name, so it reads as what it is on
// the nodes page.
func pairGroupName(one *loaders.Loader) string {
	return strings.TrimSuffix(one.Name, stage1Suffix) + stage2Suffix
}

// pairGroup returns the group Core made for the pair, or nil. It is never the
// plant's own wait group, even if somebody named one the same.
func (s *LoaderService) pairGroup(one, two *loaders.Loader) *nodes.Node {
	name := pairGroupName(one)
	if name == two.InboundSource {
		return nil
	}
	g, err := s.db.GetNodeByDotName(name)
	if err != nil || g == nil || !g.IsSynthetic {
		return nil
	}
	return g
}

// pairGroupBeforeRename returns the group Core made for the pair whose stage 1
// is loaderID, when newName renames that stage 1 — read under the current name,
// before the rename is written. nil when loaderID is not a stage 1, the name is
// unchanged, or the pair has no such group.
func (s *LoaderService) pairGroupBeforeRename(loaderID int64, newName string) (*nodes.Node, error) {
	one, two, err := s.pairOf(loaderID)
	if err != nil || one == nil || one.ID != loaderID || one.Name == newName {
		return nil, err
	}
	return s.pairGroup(one, two), nil
}

// dropPairGroup deletes the pair's group, which reparents its windows back to
// the grid (nodes.DeleteGroup).
func (s *LoaderService) dropPairGroup(one, two *loaders.Loader) error {
	g := s.pairGroup(one, two)
	if g == nil {
		return nil
	}
	if err := s.db.DeleteNodeGroup(g.ID); err != nil {
		return fmt.Errorf("drop the group %s made for %s: %w", g.Name, one.Name, err)
	}
	return nil
}

// checkStage2Window refuses a window a direct pair would have to take out of
// another group. Pull mode never parents a window, so it has nothing to refuse.
func (s *LoaderService) checkStage2Window(loaderID int64, node *nodes.Node) error {
	one, two, err := s.pairOf(loaderID)
	if err != nil || one == nil || two.ID != loaderID || two.InboundSource != "" || node.ParentID == nil {
		return err
	}
	parent, err := s.db.GetNode(*node.ParentID)
	if err != nil {
		return fmt.Errorf("read the group %s stands in: %w", node.Name, err)
	}
	if parent.Name == pairGroupName(one) {
		return nil
	}
	return fmt.Errorf("%w (%s is in %s)", ErrWindowInAnotherGroup, node.Name, parent.Name)
}

// syncPair derives where stage 1 sends a cart, and keeps a direct pair's group
// in step with stage 2's windows:
//
//   - pull mode (stage 2 has an inbound source): stage 1 sends to that group,
//     and a group Core made for direct mode is dropped.
//   - direct, no windows: nowhere (the CLEAR refuses, naming it).
//   - direct, one window: that node; a group Core made is dropped.
//   - direct, several: a plain group Core makes, holding every window.
func (s *LoaderService) syncPair(one, two *loaders.Loader) error {
	dest := two.InboundSource
	if dest != "" {
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
	if one.OutboundDest == dest {
		return nil
	}
	one.OutboundDest = dest
	return s.db.UpdateLoader(*one)
}

// groupStage2Windows parents every stage-2 window into the pair's group,
// creating it the first time, and returns its name.
func (s *LoaderService) groupStage2Windows(one, two *loaders.Loader, homes []loaders.Home) (string, error) {
	g := s.pairGroup(one, two)
	if g == nil {
		id, err := s.db.CreateNodeGroup(pairGroupName(one))
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
			return "", fmt.Errorf("stage-2 window %d: %w", h.PositionNodeID, err)
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

// syncPairOf re-derives the pair loaderID belongs to, if any.
func (s *LoaderService) syncPairOf(loaderID int64) error {
	one, two, err := s.pairOf(loaderID)
	if err != nil || one == nil {
		return err
	}
	return s.syncPair(one, two)
}

// deletePair archives both stages and drops the group Core made for a direct
// pair, which gives its windows back to the grid.
func (s *LoaderService) deletePair(one, two *loaders.Loader) error {
	if err := s.dropPairGroup(one, two); err != nil {
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
