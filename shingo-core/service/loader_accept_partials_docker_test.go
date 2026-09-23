//go:build docker

package service

import (
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
)

// TestLoaderService_AcceptPartials pins the accept_partials door: stored and
// kept on an unloader, refused on a produce loader.
//
// Update is a full-record write — every editable field is written from the
// request — so "re-saving keeps it on" is asserted by saving twice, the way the
// loaders page does when an operator edits any other field.
func TestLoaderService_AcceptPartials(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	consumeID, err := svc.Create("AP-UNLOADER", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create unloader")
	produceID, err := svc.Create("AP-LOADER", loaders.RoleProduce, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create loader")

	got, err := db.GetLoader(consumeID)
	testutil.MustNoErr(t, err, "read new unloader")
	if got.AcceptPartials {
		t.Fatal("a new unloader accepts partials; the default must keep the full-carrier rule")
	}

	for i := 1; i <= 2; i++ {
		testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: consumeID, Name: "AP-UNLOADER",
			Layout: loaders.LayoutSharedWindow, AcceptPartials: true}), "save unloader with partials")
		got, err = db.GetLoader(consumeID)
		testutil.MustNoErr(t, err, "re-read unloader")
		if !got.AcceptPartials {
			t.Fatalf("save %d: accept_partials = false, want true", i)
		}
	}

	err = svc.Update(LoaderUpdate{ID: produceID, Name: "AP-LOADER",
		Layout: loaders.LayoutSharedWindow, AcceptPartials: true})
	if !errors.Is(err, ErrAcceptPartialsProduce) {
		t.Fatalf("produce loader with accept_partials: err = %v, want ErrAcceptPartialsProduce", err)
	}
	got, err = db.GetLoader(produceID)
	testutil.MustNoErr(t, err, "re-read loader")
	if got.AcceptPartials || got.Name != "AP-LOADER" {
		t.Errorf("refused update landed: %+v", got)
	}
}

// TestLoaderService_AutoPush: stored and kept on an unloader across re-saves,
// refused on a produce loader.
func TestLoaderService_AutoPush(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)
	consumeID, err := svc.Create("APU-UNLOADER", loaders.RoleConsume, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create unloader")
	produceID, err := svc.Create("APU-LOADER", loaders.RoleProduce, loaders.LayoutSharedWindow, "", "", "", false)
	testutil.MustNoErr(t, err, "create loader")

	for i := 1; i <= 2; i++ {
		testutil.MustNoErr(t, svc.Update(LoaderUpdate{ID: consumeID, Name: "APU-UNLOADER",
			Layout: loaders.LayoutSharedWindow, AutoPush: true}), "save unloader with auto_push")
		got, err := db.GetLoader(consumeID)
		testutil.MustNoErr(t, err, "re-read unloader")
		if !got.AutoPush {
			t.Fatalf("save %d: auto_push = false, want true", i)
		}
	}
	err = svc.Update(LoaderUpdate{ID: produceID, Name: "APU-LOADER",
		Layout: loaders.LayoutSharedWindow, AutoPush: true})
	if !errors.Is(err, ErrAutoPushProduce) {
		t.Fatalf("produce loader with auto_push: err = %v, want ErrAutoPushProduce", err)
	}
	got, err := db.GetLoader(produceID)
	testutil.MustNoErr(t, err, "re-read loader")
	if got.AutoPush {
		t.Errorf("refused update landed: %+v", got)
	}
}
