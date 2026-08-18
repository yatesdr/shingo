// Package nodetree holds the node-tree recursive walks, as composable SQL.
//
// UNDER store/internal/ BECAUSE OF WHO NEEDS IT. The walks recurse on
// nodes.parent_id, so store/nodes looked like the natural home — but store/bins
// composes two of them, and a store sub-package may not import a sibling
// aggregate (depguard's store-sub-pkg-isolation, which exists so cross-aggregate
// orchestration stays at the outer store/ level). store/internal/ is that rule's
// documented exemption, and a package of pure SQL text with no state and no
// aggregate of its own is exactly what belongs there.
package nodetree

import "fmt"

// The node tree, as SQL. One definition of each walk, composed by every query
// that needs one.
//
// WHY THIS EXISTS. Seven queries across six files hand-rolled a recursive CTE
// over nodes.parent_id, and a reader checking whether two of them agreed had to
// diff SQL by eye. They did not all agree — see the THREE walks below, which are
// three different questions that were all being spelled "WITH RECURSIVE
// descendants" or "WITH RECURSIVE ancestors". Naming them separately is most of
// the value here; the deduplication is the smaller half.
//
// THREE WALKS, NOT ONE, AND THE DIFFERENCE IS LOAD-BEARING:
//
//	DescendantsOf   everything BELOW a node.        Self EXCLUDED.
//	SubtreeOf       a node AND everything below it. Self INCLUDED.
//	AncestorsOf     a node and its parent chain.    Self INCLUDED, carries depth.
//
// Descendants-vs-subtree is exactly the divergence this extraction found: the
// group-scoped empty finders exclude the group node itself (it is synthetic and
// holds no carriers), while the CATID boundary sum includes it (a boundary node
// can hold bins, and its own contents are part of the total). Both were called
// "descendants". If they had ever been unified under one name, one of them would
// have quietly changed its answer.
//
// THE PARAMETER IS AN INDEX, NOT A STRING, so a caller cannot splice anything
// into the SQL but a positional placeholder. These fragments are concatenated
// into query text; taking `$2` as an int and formatting it here makes injection
// structurally impossible rather than merely unlikely.
//
// EACH RETURNS THE WHOLE `WITH RECURSIVE ... AS (...)` CLAUSE, ready to prefix a
// SELECT. None of the current callers has a second CTE; a future one that does
// will need to fold this into a comma-separated list rather than prefix it, and
// should extract the body at that point rather than string-editing the result.
//
// NOT CYCLE-SAFE, and neither was any of the seven spellings this replaces: a
// parent_id cycle makes every one of these recurse forever. Nothing in the schema
// prevents one (there is no CHECK, and reparenting is an operator action). Left
// as it was found — making it safe is a behaviour change and belongs to whoever
// decides what a cycle should DO, not to an extraction.

// DescendantsOf names every node BELOW the one at the given parameter index.
// THE NODE ITSELF IS EXCLUDED.
//
// The CTE is named `descendants` and exposes one column, `id`.
func DescendantsOf(nodeParam int) string {
	return fmt.Sprintf(`WITH RECURSIVE descendants(id) AS (
			SELECT id FROM nodes WHERE parent_id = $%d
			UNION ALL
			SELECT n2.id FROM nodes n2 JOIN descendants d ON n2.parent_id = d.id
		)`, nodeParam)
}

// SubtreeOf names the node at the given parameter index AND every node below it.
// THE NODE ITSELF IS INCLUDED — that is the whole difference from DescendantsOf,
// and it is why they are two functions instead of a flag.
//
// The CTE is named `descendants` and exposes one column, `id`, so the two walks
// are drop-in for one another at the point of use. Which is precisely why the
// NAMES have to carry the difference: nothing in the query body downstream will.
func SubtreeOf(nodeParam int) string {
	return fmt.Sprintf(`WITH RECURSIVE descendants(id) AS (
			SELECT id FROM nodes WHERE id = $%d
			UNION ALL
			SELECT n.id FROM nodes n JOIN descendants d ON n.parent_id = d.id
		)`, nodeParam)
}

// AncestorsOf names the node at the given parameter index and its parent chain
// up to the root. Self is included at depth 0.
//
// The CTE is named `ancestors` and exposes `id`, `parent_id`, `depth`. Depth is
// what the inheritance lookups order by — "the nearest ancestor that has any
// rows" is `ORDER BY depth ASC LIMIT 1` — and it is carried even for the one
// caller that does not read it (GetRoot, which wants the row with a NULL
// parent_id). An unused derived column changes no row and no result; a second
// spelling of the walk to avoid it would.
func AncestorsOf(nodeParam int) string {
	return fmt.Sprintf(`WITH RECURSIVE ancestors AS (
			SELECT id, parent_id, 0 AS depth FROM nodes WHERE id = $%d
			UNION ALL
			SELECT n.id, n.parent_id, a.depth + 1 FROM nodes n
			JOIN ancestors a ON n.id = a.parent_id
		)`, nodeParam)
}

// AncestorsWithinDepth is AncestorsOf with the recursion BOUNDED: it stops after
// maxDepthParam steps instead of walking until the chain runs out.
//
// THE ONE CYCLE-SAFE WALK IN THIS FILE, and it is deliberately not the default.
// Every other fragment here recurses until parent_id goes NULL, so a parentage
// cycle makes them run forever — as noted at the top of this file, and as was
// already true of the seven hand-rolled walks they replaced.
//
// This variant exists for the code that ENFORCES acyclicity (nodes.CheckParentage,
// the guard on every parent write). That guard has to terminate on a database
// that is ALREADY corrupt, because a plant with a pre-existing cycle is exactly
// where it would otherwise hang — and a guard that hangs on the data it was
// written to protect against is not a guard. The bound is its own protection.
//
// It is NOT a fix for the other walks and must not be quietly swapped into them:
// silently truncating an inheritance lookup at a depth limit would answer the
// wrong ancestor rather than fail, which is worse than the hang. Making those
// safe is a behaviour change and belongs to whoever decides what a cycle should
// DO; making one impossible to create is what the guard does instead.
//
// ITS COUNTER COLUMN IS `hops`, NOT `depth`, UNLIKE AncestorsOf — so the two are
// deliberately not drop-in for one another. Two reasons, and the second is the
// one that matters: nodes.depth already means something else in this schema (a
// slot's position within its lane, which is what lane reachability compares), and
// this counts steps up the parent chain. The lane-reachability drift guard
// matches on `.depth <` precisely because a second comparison against a node's
// depth is usually a second definition of "buried" — it flagged this line when
// the column was called depth, and it was right to be suspicious of the name even
// though the comparison was a recursion bound. Renaming says what the number is
// instead of exempting the file from a guard that is doing its job.
func AncestorsWithinDepth(nodeParam, maxHopsParam int) string {
	return fmt.Sprintf(`WITH RECURSIVE ancestors AS (
			SELECT id, parent_id, 0 AS hops FROM nodes WHERE id = $%d
			UNION ALL
			SELECT n.id, n.parent_id, a.hops + 1 FROM nodes n
			JOIN ancestors a ON n.id = a.parent_id
			WHERE a.hops < $%d
		)`, nodeParam, maxHopsParam)
}
