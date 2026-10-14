package www

import "testing"

// TestGroupImportRows_OldLayoutCarriesTheCATID: a four-column sheet is the old
// layout, whose third column asked for a cat id and got one. It is read as the
// part AND its cat id, so a re-import of an old sheet cannot make a linked part
// whose only identity is a number the catalog no longer reads as a cat id. The
// five-column layout keeps its own cat id column.
func TestGroupImportRows_OldLayoutCarriesTheCATID(t *testing.T) {
	t.Parallel()
	groups := groupImportRows([][]string{
		{"OLD-PAYLOAD", "100", "33145", "2"},
		{"NEW-PAYLOAD", "100", "767D2-6SA0A.06", "33146", "3"},
	}, &importReport{})
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	old, cur := groups[0].entries[0], groups[1].entries[0]
	if old.part != "33145" || old.catid != "33145" || old.qty != "2" {
		t.Errorf("old layout entry = part %q catid %q qty %q, want 33145 / 33145 / 2", old.part, old.catid, old.qty)
	}
	if cur.part != "767D2-6SA0A.06" || cur.catid != "33146" || cur.qty != "3" {
		t.Errorf("current layout entry = part %q catid %q qty %q, want 767D2-6SA0A.06 / 33146 / 3", cur.part, cur.catid, cur.qty)
	}
}
