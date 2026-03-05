package store

// TagVerifyResult holds the result of a tag verification check.
type TagVerifyResult struct {
	Match    bool
	Expected string
	Detail   string
}

// VerifyTag performs best-effort QR tag verification for an order.
// It looks up the claimed instance, learns new tags, and logs events.
// Returns match=true even on mismatch/missing (best-effort: never blocks orders).
func (db *DB) VerifyTag(orderUUID, tagID, location string) *TagVerifyResult {
	order, err := db.GetOrderByUUID(orderUUID)
	if err != nil {
		return &TagVerifyResult{Match: true, Detail: "order not found — accepting scan"}
	}

	if order.InstanceID == nil {
		return &TagVerifyResult{Match: true, Detail: "no instance tracking — accepting scan"}
	}

	inst, err := db.GetInstance(*order.InstanceID)
	if err != nil {
		return &TagVerifyResult{Match: true, Detail: "instance not found — accepting scan"}
	}

	if inst.TagID == "" {
		// Learn the tag on first scan
		inst.TagID = tagID
		db.UpdateInstance(inst)
		db.CreateInstanceEvent(&InstanceEvent{
			InstanceID: inst.ID, EventType: InstanceEventTagScanned,
			Detail: "tag learned from scan: " + tagID, Actor: "system",
		})
		return &TagVerifyResult{Match: true, Detail: "tag learned: " + tagID}
	}

	if inst.TagID == tagID {
		db.CreateInstanceEvent(&InstanceEvent{
			InstanceID: inst.ID, EventType: InstanceEventTagScanned,
			Detail: "tag verified at " + location, Actor: "system",
		})
		return &TagVerifyResult{Match: true, Detail: "tag match"}
	}

	// Tag mismatch — best-effort: log but proceed
	db.CreateInstanceEvent(&InstanceEvent{
		InstanceID: inst.ID, EventType: InstanceEventTagMismatch,
		Detail: "expected " + inst.TagID + " got " + tagID, Actor: "system",
	})
	return &TagVerifyResult{Match: false, Expected: inst.TagID, Detail: "tag mismatch — proceeding (best-effort)"}
}
