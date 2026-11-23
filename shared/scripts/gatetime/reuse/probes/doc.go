// Package probes is the reuse probe set: the shapes that beat every earlier
// skip-work guard (work/loc-census-2026-11-20, D11, D12, D4, G11) and the ones
// the round-2 designs added, each run against `gatetime exec` with reuse on in
// a throwaway copy, each printing one line:
//
//	PROBE <n> <name> [<sources>]: want <steps>, got <steps> (<how>) -- ok|WRONG
//
// The tests are behind the reuseprobe build tag, so no gate tier and no CI job
// runs them (they build real packages and take minutes). Run them before reuse
// is switched on anywhere, and again after any change to package reuse or to
// gatetime exec, from shared/:
//
//	go test -tags reuseprobe -count=1 -v -timeout 90m ./scripts/gatetime/reuse/probes/
//
// The kill criterion is the set's, written before it first ran: every line
// prints and every line ends "-- ok". One WRONG line, or one that does not
// print, keeps reuse off. A probe's want is never edited to make it pass.
package probes
