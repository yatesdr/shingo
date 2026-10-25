//go:build docker

package service

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// loader_drains_docker_test.go — where an unloader's drain boxes may be on
// (service/loader_drains.go). The pull itself is dispatch's
// (dispatch/loader_drain_docker_test.go).

// drainGroup makes a node group under parent (0: top level).
func drainGroup(t *testing.T, db *store.DB, name string, parent int64) *nodes.Node {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	g := &nodes.Node{Name: name, NodeTypeID: &ngrp.ID, Enabled: true, IsSynthetic: true}
	if parent != 0 {
		g.ParentID = &parent
	}
	testutil.MustNoErr(t, db.CreateNode(g), "group "+name)
	testdb.MarkGroup(t, db, g.ID)
	return g
}

var leftovers = []protocol.CarrierState{protocol.CarrierLeftover}

// TestLoaderService_DrainsStoredAndKept: a single unloader with an inbound
// source stores its boxes, in Drainable's order whatever order they were sent
// in, keeps them across a save that changes another setting and sends them
// back, and drains nothing once they are sent empty. A state no box drains is
// refused.
func TestLoaderService_DrainsStoredAndKept(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	src := drainGroup(t, db, "DRS-PRESS-OUT", 0)
	id, err := svc.Create("DRS-UNL", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", src.Name, false)
	testutil.MustNoErr(t, err, "create unloader")

	got, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "read new unloader")
	if len(got.Drains) != 0 {
		t.Fatalf("a new unloader drains %v; every box must start off", got.Drains)
	}
	sent := []protocol.CarrierState{protocol.CarrierUnverified, protocol.CarrierEmpty, protocol.CarrierLeftover}
	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "DRS-UNL", Layout: loaders.LayoutSharedWindow,
		InboundSource: src.Name, Drains: sent}), "tick three boxes")
	want := loaders.DrainStates{protocol.CarrierEmpty, protocol.CarrierLeftover, protocol.CarrierUnverified}
	got, err = db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read")
	if !slices.Equal(got.Drains, want) {
		t.Fatalf("stored %v, want %v", got.Drains, want)
	}
	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "DRS-UNL-RENAMED", Layout: loaders.LayoutSharedWindow,
		InboundSource: src.Name, AutoPush: true, Drains: got.Drains}), "another setting, the boxes sent back")
	got, err = db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read after the other setting")
	if !slices.Equal(got.Drains, want) || !got.AutoPush {
		t.Fatalf("after saving another setting: drains %v auto_push %v, want %v and true", got.Drains, got.AutoPush, want)
	}
	err = svc.Update(LoaderUpdate{ID: id, Name: "DRS-UNL-RENAMED", Layout: loaders.LayoutSharedWindow,
		InboundSource: src.Name, Drains: []protocol.CarrierState{protocol.CarrierBare}})
	if !errors.Is(err, ErrNotDrainable) {
		t.Errorf("a box for the bare marker: err = %v, want ErrNotDrainable", err)
	}
	testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: id, Name: "DRS-UNL-RENAMED", Layout: loaders.LayoutSharedWindow,
		InboundSource: src.Name}), "untick every box")
	got, err = db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read after unticking")
	if len(got.Drains) != 0 {
		t.Errorf("after a save sending none: drains %v, want none", got.Drains)
	}
}

// TestLoaderService_DrainsRefusedWhereTheyCannotHold pins every place a box
// may not be on, each refused with its own error and nothing landing: a
// produce loader; an unloader fed directly, and one whose inbound source is
// cleared while a box is on; either half of a two-stage pair; and an outbound
// destination that is the inbound source, lies in it or contains it, or whose
// group's overflow destination lies in it. The same unloader with its
// outbound elsewhere saves.
func TestLoaderService_DrainsRefusedWhereTheyCannotHold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	src := drainGroup(t, db, "DRR-PRESS-OUT", 0)
	inner := drainGroup(t, db, "DRR-PRESS-OUT-INNER", src.ID)
	outer := drainGroup(t, db, "DRR-HALL", 0)
	testutil.MustNoErr(t, db.ReparentNode(src.ID, &outer.ID, 0), "the source stands in a hall group")
	elsewhere := drainGroup(t, db, "DRR-EMPTIES", 0)
	overflowing := drainGroup(t, db, "DRR-OVERFLOWING", 0)
	testutil.MustNoErr(t, db.SetNodeProperty(overflowing.ID, nodes.PropOverflowDestination, inner.Name),
		"its overflow lands in the source")

	if _, err := svc.CreateLoader(LoaderCreate{Name: "DRR-LOADER", Role: loaders.RoleProduce,
		InboundSource: src.Name, Drains: leftovers}); !errors.Is(err, ErrDrainProduce) {
		t.Errorf("a produce loader with a box: err = %v, want ErrDrainProduce", err)
	}
	if _, err := svc.CreateLoader(LoaderCreate{Name: "DRR-FED", Role: loaders.RoleConsume,
		InboundSource: src.Name, FedDirectly: true, Drains: leftovers}); !errors.Is(err, ErrDrainNoSource) {
		t.Errorf("an unloader fed directly with a box: err = %v, want ErrDrainNoSource", err)
	}
	if _, _, err := svc.CreateTwoStage(TwoStageCreate{Name: "DRR-PAIR-NEW", Drains: leftovers}); !errors.Is(err, ErrDrainTwoStage) {
		t.Errorf("a two-stage create with a box: err = %v, want ErrDrainTwoStage", err)
	}

	id, err := svc.CreateLoader(LoaderCreate{Name: "DRR-UNL", Role: loaders.RoleConsume,
		InboundSource: src.Name, OutboundDest: elsewhere.Name, Drains: leftovers})
	testutil.MustNoErr(t, err, "an unloader draining a press's outbound into the empties")
	save := func(in LoaderUpdate) error {
		in.ID, in.Name, in.Layout = id, "DRR-UNL", loaders.LayoutSharedWindow
		return svc.Update(in)
	}
	if err := save(LoaderUpdate{OutboundDest: elsewhere.Name, Drains: leftovers}); !errors.Is(err, ErrDrainNoSource) {
		t.Errorf("the inbound source cleared with a box on: err = %v, want ErrDrainNoSource", err)
	}
	if err := save(LoaderUpdate{InboundSource: src.Name, OutboundDest: elsewhere.Name, FedDirectly: true,
		Drains: leftovers}); !errors.Is(err, ErrDrainNoSource) {
		t.Errorf("fed directly ticked with a box on: err = %v, want ErrDrainNoSource", err)
	}
	for _, c := range []struct {
		name, outbound string
	}{
		{"the outbound is the source", src.Name},
		{"the outbound lies in the source", inner.Name},
		{"the outbound contains the source", outer.Name},
		{"the outbound's overflow lies in the source", overflowing.Name},
	} {
		err := save(LoaderUpdate{InboundSource: src.Name, OutboundDest: c.outbound, Drains: leftovers})
		if !errors.Is(err, ErrDrainIntoItsSource) {
			t.Errorf("%s: err = %v, want ErrDrainIntoItsSource", c.name, err)
			continue
		}
		t.Logf("%s: %v", c.name, err)
	}
	got, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read")
	if got.InboundSource != src.Name || got.OutboundDest != elsewhere.Name || !slices.Equal(got.Drains, loaders.DrainStates(leftovers)) {
		t.Errorf("a refused save landed: %+v", got)
	}

	s1, s2, err := svc.CreateTwoStage(TwoStageCreate{Name: "DRR-PAIR"})
	testutil.MustNoErr(t, err, "a pair")
	for _, half := range []int64{s1, s2} {
		l, err := db.GetLoader(half)
		testutil.MustNoErr(t, err, "read a half")
		err = svc.Update(LoaderUpdate{ID: half, Name: l.Name, Layout: loaders.LayoutSharedWindow,
			InboundSource: src.Name, Drains: leftovers})
		if !errors.Is(err, ErrDrainTwoStage) {
			t.Errorf("%s with a box: err = %v, want ErrDrainTwoStage", l.Name, err)
		}
	}
}

// TestLoaderService_DrainsFromAMaintainedSourceIsTheOneRefusal: an unloader
// whose inbound source declares a level is refused whatever its boxes, by the
// refusal every such unloader already meets (ErrFullsFromAnEmptiesBank), and
// its sentence says the boxes drain nothing from one; no second refusal is
// spelled for the boxes.
func TestLoaderService_DrainsFromAMaintainedSourceIsTheOneRefusal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	bank := drainGroup(t, db, "DRM-BANK", 0)
	bt := &bins.BinType{Code: "DRM-TOTE", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "bin type")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: bank.ID, BinTypeID: bt.ID, Want: 2}),
		"the bank's level")
	for _, drains := range [][]protocol.CarrierState{nil, leftovers} {
		_, err := svc.CreateLoader(LoaderCreate{Name: "DRM-UNL", Role: loaders.RoleConsume,
			InboundSource: bank.Name, Drains: drains})
		if !errors.Is(err, ErrFullsFromAnEmptiesBank) {
			t.Fatalf("drains %v from a maintained group: err = %v, want ErrFullsFromAnEmptiesBank", drains, err)
		}
	}
	if want := "drain boxes drain nothing"; !strings.Contains(ErrFullsFromAnEmptiesBank.Error(), want) {
		t.Errorf("the refusal reads %q; it must say the boxes drain nothing from one (%q)", ErrFullsFromAnEmptiesBank, want)
	}
}

// TestLoaderService_ADrainBoxNeedsSharedWindows: a drain moves carts into an
// unloader's shared windows, so a box on an unloader with one spot per part is
// refused, at create and at update, and a refused save lands nothing. The
// drain reads the same rule off the row (dispatch.drainsAt), so a box stored
// on a dedicated layout was kept and never pulled.
func TestLoaderService_ADrainBoxNeedsSharedWindows(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	src := drainGroup(t, db, "DRD-PRESS-OUT", 0)

	if _, err := svc.CreateLoader(LoaderCreate{Name: "DRD-NEW", Role: loaders.RoleConsume,
		Layout: loaders.LayoutDedicatedPositions, InboundSource: src.Name, Drains: leftovers}); !errors.Is(err, ErrDrainDedicatedLayout) {
		t.Errorf("a dedicated unloader created with a box: err = %v, want ErrDrainDedicatedLayout", err)
	}
	id, err := svc.CreateLoader(LoaderCreate{Name: "DRD-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutDedicatedPositions, InboundSource: src.Name})
	testutil.MustNoErr(t, err, "a dedicated unloader with no box")
	err = svc.Update(LoaderUpdate{ID: id, Name: "DRD-UNL", Layout: loaders.LayoutDedicatedPositions,
		InboundSource: src.Name, Drains: leftovers})
	if !errors.Is(err, ErrDrainDedicatedLayout) {
		t.Errorf("a box ticked on a dedicated unloader: err = %v, want ErrDrainDedicatedLayout", err)
	}
	got, err := db.GetLoader(id)
	testutil.MustNoErr(t, err, "re-read")
	if len(got.Drains) != 0 {
		t.Errorf("a refused save stored the boxes %v", got.Drains)
	}
}
