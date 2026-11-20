package www

import "shingo/protocol"

// countShown is a count as a server-drawn Core page shows it (owner,
// 2026-11-19): above its part's ratio of 1, the whole UoP where the count
// stood (protocol.SplitUOP) and its parts said small beside it; at a ratio of
// 1, the count and "", and the page reads as it always did. Readers: the
// dashboard's count anomalies, Material flags' ledger, a threshold demand
// episode's level and threshold. Twin: count-words.js countReading.
func countShown(count, ratio int) (shown int, parts string) {
	if ratio <= 1 {
		return count, ""
	}
	u, _ := protocol.SplitUOP(count, ratio)
	return u, countPartsWords(count)
}
