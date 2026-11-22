package protocol

import (
	"regexp"
	"strconv"
)

// peer_floor.go — the oldest peer releases this build supports, declared once.
//
// The floor is REPORTED, NEVER ENFORCED: nothing here refuses a peer, delays
// one or changes what is sent to it. It is the line below which this build
// owes a peer nothing: a path kept only for a release older than the floor may
// be deleted, and a peer found below it should be shown to a person (Core's
// /edges page and Core Health). A wire key a peer at the floor reads stays, and
// so does a subject it sends; a key it sends that nothing here reads is ignored
// (the decoders refuse no unknown field). PIN: TestPeerWire_*.

// The floor, by binary: the Core and Edge releases deployed at every plant
// when this build was cut. Raise one only when no plant runs anything older.
const (
	// FloorCore is the oldest Core this build's Edge supports.
	FloorCore = "v0.19.0"
	// FloorEdge is the oldest Edge this build's Core supports.
	FloorEdge = "v0.19.0"
)

// FloorStanding is where a peer's version stands against a floor.
type FloorStanding int

const (
	// FloorUnknown is a version that names no release, so it cannot be judged:
	// "dev" (a build git could not stamp), a bare commit sha (a build with no
	// release tag behind it), "" (a peer that sent none), or any other shape.
	// It is neither below the floor nor above it: a surface shows it as unknown
	// and never as either finding.
	FloorUnknown FloorStanding = iota
	// FloorBelow is a release older than the floor.
	FloorBelow
	// FloorAtOrAbove is the floor's release or a newer one.
	FloorAtOrAbove
)

// releaseRe is a version the installers stamp from a release tag
// (install-core.sh, install-edge.sh: `git describe --tags --always --dirty`,
// or the tag itself): vMAJOR.MINOR.PATCH, then optionally -N-gSHA (N commits
// past that tag) and -dirty (local edits).
var releaseRe = regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-[0-9]+-g[0-9a-f]+)?(-dirty)?$`)

// CompareToFloor says where version stands against floor (FloorCore or
// FloorEdge).
//
// HOW A VERSION COMPARES: by its release, MAJOR then MINOR then PATCH as
// numbers (v0.9.3 is below v0.19.0). A build N commits past a release tag
// (v0.19.0-3-gabc1234) is that release: it carries every change up to the tag,
// so it stands at least there. -dirty (local edits) changes nothing. Anything
// that names no release, "dev" or a bare sha among them, is FloorUnknown.
func CompareToFloor(version, floor string) FloorStanding {
	v, ok := parseRelease(version)
	f, fok := parseRelease(floor)
	if !ok || !fok {
		return FloorUnknown
	}
	for i := range v {
		if v[i] != f[i] {
			if v[i] < f[i] {
				return FloorBelow
			}
			return FloorAtOrAbove
		}
	}
	return FloorAtOrAbove
}

// parseRelease is version's MAJOR, MINOR and PATCH; ok false when it names no
// release.
func parseRelease(version string) (r [3]int, ok bool) {
	m := releaseRe.FindStringSubmatch(version)
	if m == nil {
		return r, false
	}
	for i := range r {
		r[i], _ = strconv.Atoi(m[i+1])
	}
	return r, true
}
