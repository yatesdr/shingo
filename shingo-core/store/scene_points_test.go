//go:build docker

package store

import "testing"

func TestScenePoint_UpsertInsertThenUpdate(t *testing.T) {
	db := testDB(t)

	sp := &ScenePoint{
		AreaName:       "AREA-1",
		InstanceName:   "INST-1",
		ClassName:      "RobotPose",
		PointName:      "home",
		GroupName:      "grp-a",
		Label:          "Home Pose",
		PosX:           1.0,
		PosY:           2.0,
		PosZ:           3.0,
		Dir:            0.0,
		PropertiesJSON: `{"foo":"bar"}`,
	}
	if err := db.UpsertScenePoint(sp); err != nil {
		t.Fatalf("UpsertScenePoint insert: %v", err)
	}

	all, err := db.ListScenePoints()
	if err != nil {
		t.Fatalf("ListScenePoints: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("after insert len = %d, want 1", len(all))
	}
	if all[0].ClassName != "RobotPose" {
		t.Errorf("ClassName = %q, want RobotPose", all[0].ClassName)
	}
	if all[0].PosX != 1.0 || all[0].PosY != 2.0 || all[0].PosZ != 3.0 {
		t.Errorf("pos = (%v,%v,%v), want (1,2,3)", all[0].PosX, all[0].PosY, all[0].PosZ)
	}
	if all[0].Label != "Home Pose" {
		t.Errorf("Label = %q, want %q", all[0].Label, "Home Pose")
	}

	// Re-upsert the same (area, instance) with new positions — should update
	sp.PosX = 100.0
	sp.PosY = 200.0
	sp.Label = "New Home"
	sp.ClassName = "UpdatedPose"
	if err := db.UpsertScenePoint(sp); err != nil {
		t.Fatalf("UpsertScenePoint update: %v", err)
	}
	all2, _ := db.ListScenePoints()
	if len(all2) != 1 {
		t.Fatalf("after upsert-update len = %d, want 1 (still one row)", len(all2))
	}
	if all2[0].PosX != 100.0 {
		t.Errorf("PosX after update = %v, want 100", all2[0].PosX)
	}
	if all2[0].PosY != 200.0 {
		t.Errorf("PosY after update = %v, want 200", all2[0].PosY)
	}
	if all2[0].Label != "New Home" {
		t.Errorf("Label after update = %q, want New Home", all2[0].Label)
	}
	if all2[0].ClassName != "UpdatedPose" {
		t.Errorf("ClassName after update = %q, want UpdatedPose", all2[0].ClassName)
	}
}

func TestScenePoint_ListFiltersAndDelete(t *testing.T) {
	db := testDB(t)

	points := []*ScenePoint{
		{AreaName: "AREA-A", InstanceName: "I-1", ClassName: "Waypoint", PropertiesJSON: `{}`},
		{AreaName: "AREA-A", InstanceName: "I-2", ClassName: "Dock", PropertiesJSON: `{}`},
		{AreaName: "AREA-B", InstanceName: "I-3", ClassName: "Waypoint", PropertiesJSON: `{}`},
		{AreaName: "AREA-B", InstanceName: "I-4", ClassName: "Dock", PropertiesJSON: `{}`},
	}
	for _, p := range points {
		if err := db.UpsertScenePoint(p); err != nil {
			t.Fatalf("upsert %s/%s: %v", p.AreaName, p.InstanceName, err)
		}
	}

	// ListScenePointsByClass
	waypoints, err := db.ListScenePointsByClass("Waypoint")
	if err != nil {
		t.Fatalf("ListScenePointsByClass Waypoint: %v", err)
	}
	if len(waypoints) != 2 {
		t.Errorf("waypoints len = %d, want 2", len(waypoints))
	}
	for _, p := range waypoints {
		if p.ClassName != "Waypoint" {
			t.Errorf("unexpected class %q in Waypoint filter", p.ClassName)
		}
	}

	// ListScenePointsByArea
	areaA, err := db.ListScenePointsByArea("AREA-A")
	if err != nil {
		t.Fatalf("ListScenePointsByArea AREA-A: %v", err)
	}
	if len(areaA) != 2 {
		t.Errorf("AREA-A len = %d, want 2", len(areaA))
	}
	for _, p := range areaA {
		if p.AreaName != "AREA-A" {
			t.Errorf("unexpected area %q in AREA-A filter", p.AreaName)
		}
	}

	// DeleteScenePointsByArea
	if err := db.DeleteScenePointsByArea("AREA-A"); err != nil {
		t.Fatalf("DeleteScenePointsByArea: %v", err)
	}
	remaining, _ := db.ListScenePoints()
	if len(remaining) != 2 {
		t.Fatalf("remaining len = %d, want 2 (AREA-B only)", len(remaining))
	}
	for _, p := range remaining {
		if p.AreaName != "AREA-B" {
			t.Errorf("remaining area = %q, want AREA-B", p.AreaName)
		}
	}

	// AREA-A should be empty now
	gone, _ := db.ListScenePointsByArea("AREA-A")
	if len(gone) != 0 {
		t.Errorf("AREA-A after delete len = %d, want 0", len(gone))
	}
}
