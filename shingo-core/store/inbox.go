package store

// RecordInboundMessage records a processed inbound envelope ID.
// Returns true when the message is newly recorded, false when it was already seen.
func (db *DB) RecordInboundMessage(msgID, msgType, stationID string) (bool, error) {
	res, err := db.Exec(`INSERT INTO inbox (msg_id, msg_type, station_id) VALUES ($1, $2, $3) ON CONFLICT (msg_id) DO NOTHING`,
		msgID, msgType, stationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
