package dispatch

import (
	"strings"
	"testing"
)

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
			"named the carve-out. Dig ownership has one answer now, and digOwnership carries one " +
				"value to say so."},
		{"healLaneMouth",
			"fired the folder's dig on a dweller's behalf. The dweller fires its own — see " +
				"summonOwnDigs, which took its call site."},
		{"mouthHealNeeded",
			"was the folder path's own physics read. The physics survive as acceptanceDigNeeded, " +
				"asked by the order that will do the digging."},
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

// And the durable half: the column the folder used to carry has no writer, so
// nothing can be marked as one.
//
// dig_target_node was arm 2's fact — "this folder owes a prize" — and only
// createServiceDigParent ever wrote it. It is left in the schema rather than
// migrated away in the same breath as the funeral, and this test is what stops
// that from being a quiet resurrection point: a writer appearing here means
// something is minting the shape again under a different name.
//
// READERS ARE NOT BANNED. Two survive (CollectorForDigTarget and the handoff that
// consults it), both now writer-less and both reported as such rather than
// deleted unasked. A reader of a column nothing writes answers "no" forever,
// which is correct and harmless; a WRITER would make it lie again.
func TestFence_NothingWritesTheFolderColumn(t *testing.T) {
	sources := scanCoreSources(t)
	for path, src := range sources {
		if strings.Contains(path, "migrations") || strings.Contains(path, "schema") {
			continue // the column's declaration is not a writer
		}
		// THE GO FIELD, NOT THE SQL. The canonical order INSERT names every column
		// including this one and always will — that is the one order writer doing
		// its job, not a folder minter. What carries the fact is the STRUCT FIELD,
		// and nothing sets it any more: the value reaching that INSERT is the zero
		// value on every path in the system.
		//
		// Readers are untouched (`parent.DigTargetNode == ""`, and the comparisons
		// in the handoff and the tripwire). Only an assignment is the shape coming
		// back.
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "DigTargetNode:") ||
				(strings.Contains(line, "DigTargetNode =") && !strings.Contains(line, "==")) {
				t.Errorf("%s:%d assigns DigTargetNode\n\t%s\nThe column is the folder's, the folder "+
					"is deleted, and a writer is the shape coming back wearing the old label.",
					path, i+1, trimmed)
			}
		}
	}
}
