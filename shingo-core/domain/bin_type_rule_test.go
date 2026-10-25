package domain

import "testing"

// TestBinTypeRule_PermitsATote: whether a tote carrier may hold the part. The
// zero rule (no rows) permits every type, tote carriers among them; a rule with
// rows permits one only where a type it names is a tote carrier; a part
// described as carryable by nothing permits none.
func TestBinTypeRule_PermitsATote(t *testing.T) {
	t.Parallel()
	const tote, knockdown = 1, 2
	for _, c := range []struct {
		name string
		rule BinTypeRule
		want bool
	}{
		{"no rows", NewBinTypeRule(nil, nil), true},
		{"names a tote carrier", NewBinTypeRule([]int64{knockdown, tote}, []int64{tote}), true},
		{"names no tote carrier", NewBinTypeRule([]int64{knockdown}, nil), false},
		{"a tote id the rule does not name", NewBinTypeRule([]int64{knockdown}, []int64{tote}), false},
		{"carryable by nothing", NewBinTypeRule([]int64{}, nil), false},
	} {
		if got := c.rule.PermitsATote(); got != c.want {
			t.Errorf("%s: PermitsATote = %v, want %v", c.name, got, c.want)
		}
	}
}
