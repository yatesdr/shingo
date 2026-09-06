package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArch_InboundPickupUsesRefillPickup makes refillPickup's own claim true.
//
// Its comment declares it "the ONE way to express fetch a fresh carrier from
// the inbound source" and says every builder opening a leg at InboundSource
// must use it. Five builders in material_orders.go did not: they wrote a raw
// buildStep("pickup", claim.InboundSource) and took the Empty flag from a
// post-hoc markInboundEmpty pass in BuildSwapDispatch. Three of the five also
// carried their own Role == Produce guard; two carried nothing and were correct
// only because a caller fixed them up.
//
// That is the arrangement the constructor was written to abolish — an invariant
// living at the leaf instead of at the boundary — and git log -S markInboundEmpty
// records it being re-added to one more builder after each floor stall, seven
// times.
//
// GO CANNOT MAKE THIS UNWRITEABLE. buildStep is already unexported and every
// bypass was in the same file, so there is no visibility boundary left to move
// and no rung-5 fix to reach for. A structural grep is the strongest rung
// actually available, which is what this is.
//
// Same shape as uop/archtest_test.go: os.WalkDir plus a substring check.
func TestArch_InboundPickupUsesRefillPickup(t *testing.T) {
	t.Parallel()
	root := edgeRoot(t)
	// The needle is a raw pickup step built at an inbound source. refillPickup's
	// own body is the one legitimate occurrence.
	const needle = `buildStep("pickup", claim.InboundSource)`
	const inRefillPickup = `buildStep("pickup", toClaim.InboundSource)`

	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			// A comment may name the pattern in order to forbid it, and this
			// test's own subject is a comment describing the raw form.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, needle) {
				bad = append(bad, rel+":"+itoaLine(i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("a leg opens at InboundSource without refillPickup:\n  %s\n\n"+
			"Use refillPickup(fromClaim, claim) — nil fromClaim in steady state. A raw pickup "+
			"there asks Core for a FULL bin of the incoming part inside the empty-carrier "+
			"supermarket, which no inventory can satisfy, so the order parks and retries until "+
			"somebody cancels it. That is Hopkinsville PLN_03/PLN_06.\n\n"+
			"The sole legitimate site is refillPickup's own body, which reads %q.",
			strings.Join(bad, "\n  "), inRefillPickup)
	}
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
