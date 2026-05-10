package engine

import "testing"

func TestDeriveProcessTagPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"MES_P42_Spot_Nut_Farm_2.Prod_Counter_01", "MES_P42_Spot_Nut_Farm_2"},
		{"MES_P42_Spot_Nut_Farm_2.Changeover_Active", "MES_P42_Spot_Nut_Farm_2"},
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

func TestDeriveCutoverTag(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"MES_P42_Spot_Nut_Farm_2.Prod_Counter_01", "MES_P42_Spot_Nut_Farm_2.Changeover_Active"},
		{"MES_OtherLine_Process.Prod_Counter_03", "MES_OtherLine_Process.Changeover_Active"},
		{"NoStruct", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := deriveCutoverTag(tc.in); got != tc.want {
			t.Errorf("deriveCutoverTag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
