package kanban

// line.go — one level type of a group as its line reads it. Its readers are an unloader's group
// line and the keeper's count of what the walk lights.

// Line is one level type of a group: what the group wants of the type, what it has
// (resident, asked and coming: want less Have.Short), and its wanted heights when
// the type states its totes per layer.
type Line struct {
	Code          string
	Want, Have    int
	TotesPerLayer int // 0: the type states none, and has no heights
	Heights       []HeightLine
}

// HeightLine is one wanted height of a type: how many carts standing layers high the
// group wants, and how many it has (resident and coming at exactly that many totes).
type HeightLine struct {
	Layers     int
	Want, Have int
}

// LineOf is a type's line with no heights yet, from its want and its populations.
func LineOf(code string, want int, have Have) Line {
	return Line{Code: code, Want: want, Have: want - have.Short(want)}
}

// Short is how many more of the type the group wants: want less have. At or below
// zero the group is not short of it.
func (l Line) Short() int { return l.Want - l.Have }
