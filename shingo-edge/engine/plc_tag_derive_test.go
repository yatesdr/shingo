package engine

import "testing"

func TestDeriveProcessTagPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"MES_Press_A1.Prod_Counter_01", "MES_Press_A1"},
		{"MES_Press_A1.Changeover_Active", "MES_Press_A1"},
		{"a.b.c", "a.b"},
		{"NoStructHere", ""},
		{"", ""},
		{".LeafWithEmptyParent", ""},
	}
	for _, tc := range cases {
		if got := deriveProcessTagPrefix(tc.in); got != tc.want {
			t.Errorf("deriveProcessTagPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
