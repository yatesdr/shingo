//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/loaders"
)

// TestMigrate_StageTwoProduces: v168 makes the loader a stage 1 names a
// produce loader, with accept partials and auto push off (both an unloader's),
// and leaves the stage 1 and every other loader as they were. A stage 2 written
// consume after the step (an older binary's create) is put right by the
// runner's self-heal on the next start.
func TestMigrate_StageTwoProduces(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	mk := func(l loaders.Loader) int64 {
		t.Helper()
		id, err := db.CreateLoader(l)
		if err != nil {
			t.Fatalf("create %s: %v", l.Name, err)
		}
		return id
	}
	unloader := loaders.Loader{Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
		Replenishment: loaders.ReplenishmentOperator, AcceptPartials: true, AutoPush: true}
	two := unloader
	two.Name = "MIG-S2"
	s2 := mk(two)
	one := unloader
	one.Name, one.SecondStageLoaderID = "MIG-S1", &s2
	s1 := mk(one)
	lone := unloader
	lone.Name = "MIG-LONE"
	other := mk(lone)

	if err := db.MigrateForTest(); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	for _, c := range []struct {
		id             int64
		role           string
		partials, push bool
		what           string
	}{
		{s2, loaders.RoleProduce, false, false, "stage 2"},
		{s1, loaders.RoleConsume, true, true, "stage 1"},
		{other, loaders.RoleConsume, true, true, "a lone unloader"},
	} {
		l, err := db.GetLoader(c.id)
		if err != nil {
			t.Fatalf("read %s: %v", c.what, err)
		}
		if l.Role != c.role || l.AcceptPartials != c.partials || l.AutoPush != c.push {
			t.Errorf("%s: role %s partials %v push %v, want %s %v %v", c.what, l.Role, l.AcceptPartials, l.AutoPush,
				c.role, c.partials, c.push)
		}
	}
}
