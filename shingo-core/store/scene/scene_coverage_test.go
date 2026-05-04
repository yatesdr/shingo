//go:build docker

package scene_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/scene"
)

func TestCoverage_ScenePoint_UpsertInsertThenUpdate(t *testing.T) {
	db := testdb.Open(t)
	sp := &scene.Point{AreaName: "AREA-1", InstanceName: "INST-1", ClassName: "RobotPose", PointName: "home", GroupName: "grp-a", Label: "Home Pose", PosX: 1.0, PosY: 2.0, PosZ: 3.0, Dir: 0.0, PropertiesJSON: `{"foo":"bar"}`}
	if err := scene.Upsert(db.DB, sp); err != nil { t.Fatalf("Upsert insert: %v", err) }
	all, err := scene.List(db.DB)
	if err != nil { t.Fatalf("List: %v", err) }
	if len(all) != 1 { t.Fatalf("after insert len = %d, want 1", len(all)) }
	if all[0].ClassName != "RobotPose" { t.Errorf("ClassName = %q, want RobotPose", all[0].ClassName) }
	if all[0].PosX != 1.0 || all[0].PosY != 2.0 || all[0].PosZ != 3.0 { t.Errorf("pos = (%v,%v,%v), want (1,2,3)", all[0].PosX, all[0].PosY, all[0].PosZ) }
	if all[0].Label != "Home Pose" { t.Errorf("Label = %q, want Home Pose", all[0].Label) }
	sp.PosX = 100.0; sp.PosY = 200.0; sp.Label = "New Home"; sp.ClassName = "UpdatedPose"
	if err := scene.Upsert(db.DB, sp); err != nil { t.Fatalf("Upsert update: %v", err) }
	all2, _ := scene.List(db.DB)
	if len(all2) != 1 { t.Fatalf("after upsert-update len = %d, want 1", len(all2)) }
	if all2[0].PosX != 100.0 { t.Errorf("PosX after update = %v, want 100", all2[0].PosX) }
	if all2[0].PosY != 200.0 { t.Errorf("PosY after update = %v, want 200", all2[0].PosY) }
	if all2[0].Label != "New Home" { t.Errorf("Label after update = %q, want New Home", all2[0].Label) }
	if all2[0].ClassName != "UpdatedPose" { t.Errorf("ClassName after update = %q, want UpdatedPose", all2[0].ClassName) }
}

func TestCoverage_ScenePoint_ListFiltersAndDelete(t *testing.T) {
	db := testdb.Open(t)
	points := []*scene.Point{
		{AreaName: "AREA-A", InstanceName: "I-1", ClassName: "Waypoint", PropertiesJSON: `{}`},
		{AreaName: "AREA-A", InstanceName: "I-2", ClassName: "Dock", PropertiesJSON: `{}`},
		{AreaName: "AREA-B", InstanceName: "I-3", ClassName: "Waypoint", PropertiesJSON: `{}`},
		{AreaName: "AREA-B", InstanceName: "I-4", ClassName: "Dock", PropertiesJSON: `{}`},
	}
	for _, p := range points { if err := scene.Upsert(db.DB, p); err != nil { t.Fatalf("upsert %s/%s: %v", p.AreaName, p.InstanceName, err) } }
	waypoints, err := scene.ListByClass(db.DB, "Waypoint")
	if err != nil { t.Fatalf("ListByClass Waypoint: %v", err) }
	if len(waypoints) != 2 { t.Errorf("waypoints len = %d, want 2", len(waypoints)) }
	for _, p := range waypoints { if p.ClassName != "Waypoint" { t.Errorf("unexpected class %q", p.ClassName) } }
	areaA, err := scene.ListByArea(db.DB, "AREA-A")
	if err != nil { t.Fatalf("ListByArea AREA-A: %v", err) }
	if len(areaA) != 2 { t.Errorf("AREA-A len = %d, want 2", len(areaA)) }
	for _, p := range areaA { if p.AreaName != "AREA-A" { t.Errorf("unexpected area %q", p.AreaName) } }
	if err := scene.DeleteByArea(db.DB, "AREA-A"); err != nil { t.Fatalf("DeleteByArea: %v", err) }
	remaining, _ := scene.List(db.DB)
	if len(remaining) != 2 { t.Fatalf("remaining len = %d, want 2 (AREA-B only)", len(remaining)) }
	for _, p := range remaining { if p.AreaName != "AREA-B" { t.Errorf("remaining area = %q, want AREA-B", p.AreaName) } }
	gone, _ := scene.ListByArea(db.DB, "AREA-A")
	if len(gone) != 0 { t.Errorf("AREA-A after delete len = %d, want 0", len(gone)) }
}
