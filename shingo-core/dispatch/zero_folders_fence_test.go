package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// bannedFolderSymbolsDigest pins the ban list below.
//
// Two of the banned symbols contain the substring "ServiceDig", so a tree-wide
// substring rename of serviceDig rewrites the list to ban names that never
// existed. The fence then passes on every run while the machinery it guards is
// free to come back — it fails green, which is the one way a guard must not
// fail. That has nearly happened twice.
//
// The digest covers the symbols only, so reformatting and comment edits are
// free. Changing what is banned is legitimate: update this constant in the same
// commit. What this stops is the list changing as a side effect.
const bannedFolderSymbolsDigest = "88ccd92518fd3467e9a4ed3472880d8c541c3a76aad1765340fb8b2379ee1575"

// THE FENCE — zero folders, no exceptions clause (§R.104).
//
// A FOLDER is an order minted to OWN an excavation on somebody else's behalf: no
// bin, no payload, no demand, no destination once its plan is gone. It existed
// for one reason — {staged → reshuffling} is not a legal transition, so a robot
// dwelling at a mark was held to be incapable of owning the dig it needed.
//
// §R.104 reversed that in writing: the rounds conflated re-planning with
// resuming. A staged order owns its dig without moving at all, because its resume
// is the splice-append rather than the queue round-trip that would have needed
// the transition. With that, the last folder in the system had no reason left to
// exist, and the whole minting path is deleted.
//
// ── WHY A FENCE AND NOT A COMMENT ─────────────────────────────────────────
//
// The recognition predicate ("a parent is anything with children") was built to
// TELL a folder from a demand, because both wore the same status and nothing
// could distinguish them. Inverted, it becomes the statement that there is
// nothing to distinguish: no folder can be created, so no reader ever has to ask.
//
// This is the arm-2 lesson applied to a deletion. A subsystem removed on the
// strength of "nothing calls it any more" comes back the first time somebody
// needs the shape it had; a subsystem removed with a fence in front of it comes
// back only through an argument, which is the point.
//
// NO EXCEPTIONS CLAUSE, deliberately. The previous version of this rule had one —
// the gate-dweller carve-out, justified on physics, endorsed 5/5 by two review
// rounds — and it was wrong. A rule with a carve-out is a rule whose carve-out
// grows; the honest form of "this shape is gone" has no room in it for the shape
// coming back quietly.
func TestFence_NoFolderCanEverBeMinted(t *testing.T) {
	t.Parallel()

	sources := scanCoreSources(t)

	// The minting machinery, by name. Each entry is a symbol that existed solely
	// to create or describe a folder; a tree containing any of them again is a
	// tree where the shape has returned.
	banned := []struct {
		symbol string
		why    string
	}{
		{"createServiceDigParent",
			"minted the synthetic parent. A dig's parent is the demand that caused it — an order " +
				"that already existed before the excavation was thought of."},
		{"abandonServiceDigParent",
			"cancelled a half-written folder. Nothing can be half-written when nothing is written."},
		{"digOwnedByFolder",
			"named the carve-out. Dig ownership has one answer — the demand that caused the dig — " +
				"so there is no ownership parameter to carry a second one."},
		{"healLaneMouth",
			"fired the folder's dig on a dweller's behalf. The dweller fires its own — see " +
				"summonOwnDigs, which took its call site."},
		{"mouthHealNeeded",
			"was the folder path's own physics read. The physics survive as acceptanceDigNeeded, " +
				"asked by the order that will do the digging."},
	}

	// Checked before the scan: a scan against a rewritten ban list reports
	// success, which is worse than not scanning. See bannedFolderSymbolsDigest.
	h := sha256.New()
	for _, b := range banned {
		h.Write([]byte(b.symbol))
		h.Write([]byte{0}) // separator, so {"ab","c"} and {"a","bc"} differ
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != bannedFolderSymbolsDigest {
		var names []string
		for _, b := range banned {
			names = append(names, b.symbol)
		}
		// Spelled in pieces so a substring sweep cannot rewrite the advice that
		// explains the sweep, and send the reader looking for the wrong name.
		stem := "service" + "Dig"
		t.Fatalf("the ban list changed, so the fence below is not the one that was ruled.\n"+
			"  symbols:  %s\n  digest:   %s\n  expected: %s\n\n"+
			"Renaming? Two of these contain the substring %q, so a substring replace rewrites "+
			"the list to ban names that never existed and the fence passes forever after. "+
			"Anchor the rename (\\b%s(?=[A-Z])) and leave the list alone.\n"+
			"Changing what is banned? Update bannedFolderSymbolsDigest in the same commit.",
			strings.Join(names, ", "), got, bannedFolderSymbolsDigest,
			strings.ToUpper(stem[:1])+stem[1:], stem)
	}

	// CODE, NOT PROSE. Every one of these names survives in a TOMBSTONE — the
	// comment explaining what used to be here and why it is not — and that is the
	// record working, not the shape returning. So comment lines are skipped and
	// only live source is read. A fence that fired on its own epitaph would be
	// deleted inside a week, which is the cry-wolf failure this house names.
	for _, b := range banned {
		for path, src := range sources {
			for i, line := range strings.Split(src, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				if !strings.Contains(line, b.symbol) {
					continue
				}
				t.Errorf("%s is back, at %s:%d\n\t%s\nIt %s\nIf a folder is genuinely needed again "+
					"that is an owner ruling and a law-14 argument against R.104, not a symbol "+
					"reappearing in a diff.",
					b.symbol, path, i+1, strings.TrimSpace(line), b.why)
			}
		}
	}
}

// The BAN LIST above is this fence: those names can come back as new code, which
// is the only way the folder returns.
