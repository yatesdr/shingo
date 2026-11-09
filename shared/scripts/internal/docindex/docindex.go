// Package docindex is the index the doc-citation gate reads markdown through:
// which docs are in scope, how a doc is read, and the stamp under its title.
//
// WHY ONE HOME. Three readers (shared/scripts/testcite, deadident and
// repolint) and shared/scripts/docstamp all read the same docs. If each
// listed them, a doc another stream adds would be read by some and not
// others; if each read markdown its own way, a code span one of them sees
// across a line wrap would be two half-spans to another. The scope is by
// pattern, so a doc added or moved under one of them is read without an edit
// here.
//
// WHAT A DOC YIELDS, each with its line numbers:
//
//   - prose: the text outside fenced code blocks and HTML comments, a block
//     at a time (a paragraph, a list item, a heading, a table row), so a
//     phrase that wraps is read whole;
//   - inline code spans, read the way markdown reads them: a span may wrap
//     across lines, and a run of N backticks closes only on a run of N;
//   - fenced code blocks, with their info string;
//   - link targets: [t](x), ![a](x), reference definitions [x]: y, and the
//     src, srcset and href attributes of HTML tags;
//   - HTML comments are not read, except a docgen marker
//     <!-- generated: <source> ... -->, whose first word is a source path.
//
// In the prose text, link destinations, HTML tags, autolinks and comments are
// blanked to spaces (offsets and lines stay put), so a URL is not read as
// prose; code spans stay, backticks included, as a Go comment's do.
//
// THE STAMP is the first non-blank line after the doc's title (its first ATX
// heading, any level: the UI style-guide chapters title with ##), exactly
// "Verified against: <hex sha>". A doc with no heading has nowhere to carry
// one, and that is a finding of its own.
package docindex

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Modules are the five Go modules.
var Modules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

// Scope is the docs the gate reads: tracked files matching one of these
// patterns. `**` is any number of directories, `*` one path component.
var Scope = []string{
	"docs/**/*.md",
	"README.md",
	"README.dev.md",
	"plants/README.md",
	"shingo-core/README.md",
	"shingo-core/deploy/README.md",
	"shingo-core/docs/*.md",
	"shingo-edge/README.md",
	"shingo-edge/docs/*.md",
	"protocol/migrate/README.md",
}

// Exempt docs are not read at all. Each is a gravestone by design: it names
// what is gone because recording that is its job.
var Exempt = map[string]string{
	"CHANGELOG.md":                      "the release log: an entry names what its release removed or renamed",
	"docs/testing/population-ledger.md": "the test-population ledger: it records retired tests and the lines they stood on",
}

// InScope reports whether a tracked path (slash-separated, from the repo
// root) is a doc the gate reads.
func InScope(p string) bool {
	if _, ok := Exempt[p]; ok {
		return false
	}
	for _, pat := range Scope {
		if match(strings.Split(pat, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

// match matches path components against pattern components, `**` standing
// for zero or more of them.
func match(pat, p []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if match(pat[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], p[0]); err != nil || !ok {
			return false
		}
		pat, p = pat[1:], p[1:]
	}
	return len(p) == 0
}

// Block is a run of text with the file line each of its lines is on.
type Block struct {
	// Text is the block's lines joined by \n. In prose, inline code is kept
	// with its backticks, and link destinations, HTML tags, autolinks and
	// HTML comments are blanked to spaces. A fence body is verbatim.
	Text  string
	Lines []int // Lines[i] is the file line of text line i
	Code  []Code
}

// LineAt is the file line holding byte offset off of b.Text.
func (b Block) LineAt(off int) int {
	return b.Lines[strings.Count(b.Text[:off], "\n")]
}

// Bare is b.Text with every inline code span blanked to spaces, line breaks
// kept: the prose alone.
func (b Block) Bare() string {
	out := []byte(b.Text)
	for _, c := range b.Code {
		blank(out, c.Start, c.End)
	}
	return string(out)
}

// TextLine is text line i of b.Text.
func (b Block) TextLine(i int) string {
	return strings.Split(b.Text, "\n")[i]
}

// Code is one inline code span.
type Code struct {
	Start, End int    // the span, backticks included, as offsets in its Block's Text
	Text       string // the content as markdown reads it: a line break is a space
	Line       int    // the file line it starts on
}

// Fence is one fenced code block.
type Fence struct {
	Info string // the info string after the opening fence, trimmed
	Line int    // the opening fence's line
	Body Block  // the lines between the fences
}

// RefKind is where a link target was written.
type RefKind int

// The kinds of link target.
const (
	Link       RefKind = iota // [t](x)
	Image                     // ![a](x)
	Definition                // [x]: y
	HTMLAttr                  // src=, srcset= or href= in an HTML tag
)

// Ref is one link target, or one generated-marker source.
type Ref struct {
	Line   int
	Target string
	Kind   RefKind
}

// Doc is one markdown file as the gate reads it.
type Doc struct {
	Path    string   // slash-separated, from the repo root
	Lines   []string // the raw lines, a trailing \r dropped
	Title   int      // the 1-based line of the first ATX heading; 0 when there is none
	Prose   []Block
	Fences  []Fence
	Links   []Ref
	Sources []Ref // the source path of each <!-- generated: <source> --> marker
}

var (
	atxRe       = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t]|$)`)
	refDefRe    = regexp.MustCompile(`^ {0,3}\[([^\]^][^\]]*)\]:[ \t]*(<[^>]*>|\S+)`)
	listItemRe  = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])(?:[ \t]|$)`)
	generatedRe = regexp.MustCompile(`^<!--\s*generated:\s*(\S+)`)
	tagRe       = regexp.MustCompile(`^</?[A-Za-z][A-Za-z0-9-]*(?:[\s/]|>)`)
	autolinkRe  = regexp.MustCompile(`^<(?:[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*|[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9.-]+)>`)
	attrRe      = regexp.MustCompile(`(?i)\b(src|srcset|href)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	stampRe     = regexp.MustCompile(`^Verified against: ([0-9a-f]{4,40})$`)
)

// StampPrefix opens the stamp line.
const StampPrefix = "Verified against: "

// Parse reads one doc.
func Parse(p string, data []byte) *Doc {
	text := strings.TrimPrefix(string(data), "\uFEFF")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	d := &Doc{Path: p, Lines: lines}
	var cur []int
	flush := func() {
		if len(cur) > 0 {
			d.prose(cur)
			cur = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimLeft(l, " \t")
		if ch, n, info, ok := fenceOpen(t); ok {
			flush()
			f := Fence{Info: info, Line: i + 1}
			var body []int
			for i++; i < len(lines) && !fenceClose(strings.TrimLeft(lines[i], " \t"), ch, n); i++ {
				body = append(body, i)
			}
			f.Body = d.block(body)
			d.Fences = append(d.Fences, f)
			continue
		}
		if strings.HasPrefix(t, "<!--") && len(l)-len(t) <= 3 {
			// An HTML block: it runs to the line that closes the comment.
			flush()
			start, rest := i, t[4:]
			for !strings.Contains(rest, "-->") && i+1 < len(lines) {
				i++
				rest = lines[i]
			}
			d.comment(strings.Join(lines[start:i+1], "\n"), start+1)
			continue
		}
		if strings.TrimSpace(l) == "" {
			flush()
			continue
		}
		if m := refDefRe.FindStringSubmatch(l); m != nil {
			flush()
			d.Links = append(d.Links, Ref{i + 1, strings.Trim(m[2], "<>"), Definition})
			continue
		}
		heading := atxRe.MatchString(l)
		if heading && d.Title == 0 {
			d.Title = i + 1
		}
		row := strings.HasPrefix(t, "|")
		if heading || row || listItemRe.MatchString(t) {
			flush() // a heading, a table row and a list item each start a block
		}
		cur = append(cur, i)
		if heading || row {
			flush() // and a heading and a table row end it: a span never crosses one
		}
	}
	flush()
	return d
}

// fenceOpen reads an opening code fence from a line with its indent removed.
func fenceOpen(t string) (ch byte, n int, info string, ok bool) {
	if len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return 0, 0, "", false
	}
	ch = t[0]
	for n < len(t) && t[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	info = strings.TrimSpace(t[n:])
	if ch == '`' && strings.Contains(info, "`") {
		return 0, 0, "", false // inline code, not a fence
	}
	return ch, n, info, true
}

// fenceClose reports whether a line with its indent removed closes a fence
// opened by n of ch.
func fenceClose(t string, ch byte, n int) bool {
	k := 0
	for k < len(t) && t[k] == ch {
		k++
	}
	return k >= n && strings.TrimSpace(t[k:]) == ""
}

// block joins the given lines (indexes into d.Lines) into a Block.
func (d *Doc) block(idx []int) Block {
	var b Block
	var texts []string
	for _, i := range idx {
		texts = append(texts, d.Lines[i])
		b.Lines = append(b.Lines, i+1)
	}
	b.Text = strings.Join(texts, "\n")
	return b
}

// comment records what an HTML comment carries: a docgen marker's source.
func (d *Doc) comment(text string, line int) {
	if m := generatedRe.FindStringSubmatch(strings.TrimLeft(text, " \t")); m != nil {
		d.Sources = append(d.Sources, Ref{line, strings.TrimSuffix(m[1], "-->"), Link})
	}
}

// blank replaces s[i:j] with spaces, line breaks kept.
func blank(s []byte, i, j int) {
	for k := i; k < j; k++ {
		if s[k] != '\n' {
			s[k] = ' '
		}
	}
}

// prose reads one prose block: its code spans, link targets, HTML tags and
// comments.
func (d *Doc) prose(idx []int) {
	b := d.block(idx)
	s := b.Text
	out := []byte(s)
	// A table row is split into cells before its spans are read (GFM): a
	// stray backtick in one cell does not open a span into the next.
	row := strings.HasPrefix(strings.TrimLeft(s, " \t"), "|")
	depth := 0 // open [ brackets
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && s[i+1] != '\n':
			i += 2 // an escaped character is literal
		case c == '`':
			n := 1
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			j := closingRun(s, i+n, n, row)
			if j < 0 {
				i += n // no closing run: the backticks are literal
				continue
			}
			b.Code = append(b.Code, Code{Start: i, End: j + n, Text: codeText(s[i+n : j]), Line: b.LineAt(i)})
			i = j + n
		case c == '<':
			i = d.angle(b, out, i)
		case c == '[':
			depth++
			i++
		case c == ']' && depth > 0:
			depth--
			i = d.link(b, out, i)
		default:
			i++
		}
	}
	b.Text = string(out)
	d.Prose = append(d.Prose, b)
}

// angle reads what opens with the `<` at offset i of b.Text: an HTML comment,
// an autolink or an HTML tag, each blanked in out. It returns the offset to
// read on from.
func (d *Doc) angle(b Block, out []byte, i int) int {
	s := b.Text
	if strings.HasPrefix(s[i:], "<!--") {
		j := strings.Index(s[i+4:], "-->")
		if j < 0 {
			return i + 1
		}
		end := i + 4 + j + 3
		d.comment(s[i:end], b.LineAt(i))
		blank(out, i, end)
		return end
	}
	if m := autolinkRe.FindString(s[i:]); m != "" {
		blank(out, i, i+len(m))
		return i + len(m)
	}
	// A tag closes on its own line, before another `<`: prose such as
	// "a <b and c> d" must not blank a sentence.
	if tagRe.MatchString(s[i:]) {
		if j := strings.IndexAny(s[i+1:], "<>\n"); j >= 0 && s[i+1+j] == '>' {
			d.attrs(s[i:i+j+2], b, i)
			blank(out, i, i+j+2)
			return i + j + 2
		}
	}
	return i + 1
}

// link reads a link destination after the `]` at offset i of b.Text, when
// one follows, and blanks it in out. It returns the offset to read on from.
func (d *Doc) link(b Block, out []byte, i int) int {
	s := b.Text
	if i+1 >= len(s) || s[i+1] != '(' {
		return i + 1
	}
	end, dest, ok := linkDest(s, i+2)
	if !ok {
		return i + 1
	}
	kind := Link
	if open := strings.LastIndex(s[:i], "["); open > 0 && s[open-1] == '!' {
		kind = Image
	}
	if dest != "" {
		d.Links = append(d.Links, Ref{b.LineAt(i), dest, kind})
	}
	blank(out, i+1, end)
	return end
}

// closingRun is the offset of the next run of exactly n backticks in s at or
// after from, or -1; in a table row, -1 at the next unescaped |, where the
// cell ends.
func closingRun(s string, from, n int, row bool) int {
	for i := from; i < len(s); {
		if row && s[i] == '|' && s[i-1] != '\\' {
			return -1
		}
		if s[i] != '`' {
			i++
			continue
		}
		k := 0
		for i+k < len(s) && s[i+k] == '`' {
			k++
		}
		if k == n {
			return i
		}
		i += k
	}
	return -1
}

// codeText is a code span's content as markdown reads it: a line break is a
// space, and one space is stripped from each end when both ends have one.
func codeText(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) >= 2 && s[0] == ' ' && s[len(s)-1] == ' ' && strings.TrimSpace(s) != "" {
		s = s[1 : len(s)-1]
	}
	return s
}

// linkDest reads a link destination and optional title from s[p:], which
// follows `](`. It returns the offset just past the closing `)`.
func linkDest(s string, p int) (end int, dest string, ok bool) {
	skip := func() {
		for p < len(s) && (s[p] == ' ' || s[p] == '\t' || s[p] == '\n') {
			p++
		}
	}
	skip()
	if p < len(s) && s[p] == '<' {
		j := strings.IndexAny(s[p+1:], ">\n")
		if j < 0 || s[p+1+j] != '>' {
			return 0, "", false
		}
		dest, p = s[p+1:p+1+j], p+j+2
	} else {
		start, depth := p, 0
		for ; p < len(s); p++ {
			c := s[p]
			if c == '\\' && p+1 < len(s) {
				p++
				continue
			}
			if c == ' ' || c == '\t' || c == '\n' || (c == ')' && depth == 0) {
				break
			}
			if c == '(' {
				depth++
			} else if c == ')' {
				depth--
			}
		}
		dest = s[start:p]
	}
	skip()
	if p < len(s) && (s[p] == '"' || s[p] == '\'' || s[p] == '(') {
		closer := s[p]
		if closer == '(' {
			closer = ')'
		}
		j := strings.IndexByte(s[p+1:], closer)
		if j < 0 {
			return 0, "", false
		}
		p += j + 2
		skip()
	}
	if p >= len(s) || s[p] != ')' {
		return 0, "", false
	}
	return p + 1, dest, true
}

// attrs records the src, srcset and href targets of one HTML tag, which
// starts at offset at of b.Text.
func (d *Doc) attrs(tag string, b Block, at int) {
	for _, m := range attrRe.FindAllStringSubmatchIndex(tag, -1) {
		v := ""
		for g := 4; g <= 8; g += 2 {
			if m[g] >= 0 {
				v = tag[m[g]:m[g+1]]
			}
		}
		line := b.LineAt(at + m[0])
		if strings.EqualFold(tag[m[2]:m[3]], "srcset") {
			for _, cand := range strings.Split(v, ",") {
				if f := strings.Fields(cand); len(f) > 0 {
					d.Links = append(d.Links, Ref{line, f[0], HTMLAttr})
				}
			}
			continue
		}
		if v != "" {
			d.Links = append(d.Links, Ref{line, v, HTMLAttr})
		}
	}
}

// Stamp returns the sha on the doc's stamp line and its 1-based line. ok is
// false when the first non-blank line after the title is not a stamp, or
// there is no title.
func (d *Doc) Stamp() (sha string, line int, ok bool) {
	if d.Title == 0 {
		return "", 0, false
	}
	for i := d.Title; i < len(d.Lines); i++ {
		if strings.TrimSpace(d.Lines[i]) == "" {
			continue
		}
		if m := stampRe.FindStringSubmatch(d.Lines[i]); m != nil {
			return m[1], i + 1, true
		}
		return "", 0, false
	}
	return "", 0, false
}

// SetStamp returns data with its stamp set to sha: the stamp line replaced
// when there is one, and otherwise inserted under the title as a blank line
// and the stamp line, with a blank line after it when the next line is not
// blank. The line endings of the title line are kept.
func SetStamp(p string, data []byte, sha string) ([]byte, error) {
	d := Parse(p, data)
	if d.Title == 0 {
		return nil, fmt.Errorf("%s: no title to stamp under", p)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	eol := func(l string) string {
		switch {
		case strings.HasSuffix(l, "\r\n"):
			return "\r\n"
		case strings.HasSuffix(l, "\n"):
			return "\n"
		}
		return ""
	}
	stamp := StampPrefix + sha
	if _, at, ok := d.Stamp(); ok {
		lines[at-1] = stamp + eol(lines[at-1])
		return []byte(strings.Join(lines, "")), nil
	}
	title := lines[d.Title-1]
	nl := eol(title)
	if nl == "" {
		nl = "\n"
		title += nl
	}
	ins := nl + stamp + nl
	if d.Title < len(lines) && strings.TrimSpace(lines[d.Title]) != "" {
		ins += nl
	}
	out := append([]string{}, lines[:d.Title-1]...)
	out = append(out, title+ins)
	out = append(out, lines[d.Title:]...)
	return []byte(strings.Join(out, "")), nil
}

// Index is the tracked tree and its in-scope docs.
type Index struct {
	Root    string
	Tracked []string // every tracked path, sorted
	Docs    []*Doc   // the in-scope docs present in the working tree, sorted by path
}

// Tracked lists every tracked path under root, slash-separated and sorted:
// `git ls-files`, the answer a clean CI checkout gets.
func Tracked(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// Load lists the tracked tree under root and reads every in-scope doc. A
// tracked doc missing from the working tree is skipped, as git grep skips it.
func Load(root string) (*Index, error) {
	tracked, err := Tracked(root)
	if err != nil {
		return nil, err
	}
	ix := &Index{Root: root, Tracked: tracked}
	for _, p := range tracked {
		if !InScope(p) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			continue
		}
		ix.Docs = append(ix.Docs, Parse(p, data))
	}
	return ix, nil
}
