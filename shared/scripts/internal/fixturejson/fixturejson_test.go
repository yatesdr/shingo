package fixturejson

import (
	"math"
	"testing"
)

func TestFormatFloat(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{125, "125.0"},
		{2.5, "2.5"},
		{0.0001, "0.0001"},
		{0.00001, "1e-05"},
		{1.5e-7, "1.5e-07"},
		{9999999999999998, "9999999999999998.0"},
		{1e16, "1e+16"},
		{1.25e22, "1.25e+22"},
		{0.30000000000000004, "0.30000000000000004"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.NaN(), "NaN"},
	} {
		if got := FormatFloat(c.f); got != c.want {
			t.Errorf("FormatFloat(%v) = %s, want %s", c.f, got, c.want)
		}
	}
}

// Round goes to the nearest decimal of the EXACT binary value: 2.675 is
// stored just below 2.675, so it rounds down; 0.125 is exact, a tie, and
// goes to the even digit.
func TestRound(t *testing.T) {
	for _, c := range []struct {
		f    float64
		n    int
		want float64
	}{
		{2.675, 2, 2.67},
		{0.125, 2, 0.12},
		{0.375, 2, 0.38},
		{118.9995, 3, 118.999},
		{-0.0004, 3, math.Copysign(0, -1)},
	} {
		got := Round(c.f, c.n)
		if got != c.want || math.Signbit(got) != math.Signbit(c.want) {
			t.Errorf("Round(%v, %d) = %v, want %v", c.f, c.n, got, c.want)
		}
	}
}

func TestParseAndPrint(t *testing.T) {
	in := `{"b": 1, "a": [1.0, -0, 2E3, 123456789012345678901234567890, null, true],` +
		` "s": "q\"\\\n\u0001\u007fé😀", "b": 2, "e": {}, "l": []}`
	v, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"compact", Compact(v, false),
			`{"b":2,"a":[1.0,0,2000.0,123456789012345678901234567890,null,true],"s":"q\"\\\n\u0001` + "\x7fé😀" + `","e":{},"l":[]}`},
		{"spaced ascii", Spaced(v, true),
			`{"b": 2, "a": [1.0, 0, 2000.0, 123456789012345678901234567890, null, true], "s": "q\"\\\n\u0001\u007f\u00e9\ud83d\ude00", "e": {}, "l": []}`},
		{"indented sorted", Indented(v, 1, true, true),
			"{\n \"a\": [\n  1.0,\n  0,\n  2000.0,\n  123456789012345678901234567890,\n  null,\n  true\n ],\n \"b\": 2,\n \"e\": {},\n \"l\": [],\n" +
				` "s": "q\"\\\n\u0001\u007f\u00e9\ud83d\ude00"` + "\n}"},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, c.got, c.want)
		}
	}
}

func TestParse_RefusesTrailingData(t *testing.T) {
	if _, err := Parse([]byte(`{} {}`)); err == nil {
		t.Error("two documents parsed as one")
	}
}

func TestObj_SetAppendsAndDeleteKeepsOrder(t *testing.T) {
	o := NewObj()
	for _, k := range []string{"a", "b", "c"} {
		o.Set(k, Str(k))
	}
	o.Set("b", Str("B"))
	o.Set("d", NullValue())
	o.Delete("a")
	if got := Compact(ObjValue(o), false); got != `{"b":"B","c":"c","d":null}` {
		t.Errorf("got %s", got)
	}
}
