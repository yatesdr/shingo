package assets

import (
	"bytes"
	"encoding/json"
	"html/template"
	"path"
	"strings"
)

// ImportMap maps the plain URL of every module the entries can reach to its
// hashed URL: the entries themselves, every static `import … from '…'`,
// `import '…'` and `export … from '…'`, and every `import('…')` whose argument
// is one string literal, followed through every mount the handler serves.
//
// WHY THE PLAIN URL IS THE KEY. A page's tags name modules by URL (hashed,
// through URL), and modules import each other by their plain URLs, absolute
// or relative. Mapping each plain URL to the hashed one makes a module reached
// by a tag and by an import one URL, so one instance. A browser without import
// maps (Chromium before 89) ignores the map and fetches the plain URLs, which
// answer with the same bytes under ETag revalidation: no page may depend on
// the map to resolve.
//
// An entry may be given plain or hashed. One the handler does not serve is
// skipped, as is a specifier it does not serve (a bare name, a full URL, a
// computed import).
func (h *Handler) ImportMap(entries ...string) map[string]string {
	seen := map[string]bool{}
	var stack []string
	for _, e := range entries {
		if h.files[e] == nil {
			if plain, _, ok := splitHashed(e); ok {
				e = plain
			}
		}
		stack = append(stack, e)
	}
	for len(stack) > 0 {
		u := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		f := h.files[u]
		if f == nil || seen[u] {
			continue
		}
		seen[u] = true
		stack = append(stack, f.deps...)
	}
	out := make(map[string]string, len(seen))
	for u := range seen {
		out[u] = h.URL(u)
	}
	return out
}

// ImportMapTag is ImportMap as the <script type="importmap"> element a page's
// head carries, before its first module script. It is built once per entry
// list: templates call it with literal entries, so the list of lists is as
// long as the list of pages.
func (h *Handler) ImportMapTag(entries ...string) template.HTML {
	key := strings.Join(entries, "\n")
	if v, ok := h.maps.Load(key); ok {
		return v.(template.HTML)
	}
	// json.Marshal sorts the keys and escapes <, > and &, so the element
	// cannot be closed early by a URL.
	b, _ := json.Marshal(struct {
		Imports map[string]string `json:"imports"`
	}{h.ImportMap(entries...)})
	tag := template.HTML(`<script type="importmap">` + string(b) + `</script>`)
	h.maps.Store(key, tag)
	return tag
}

// FuncMap is the template functions over this handler, for both routers'
// FuncMaps: assetURL (URL) and importMap (ImportMapTag).
func (h *Handler) FuncMap() template.FuncMap {
	return template.FuncMap{
		"assetURL":  h.URL,
		"importMap": h.ImportMapTag,
	}
}

// resolveDeps turns a module's specifiers into the plain URLs it imports that
// the handler serves, in order, each once.
func (h *Handler) resolveDeps(from string, specs []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range specs {
		u := resolveSpecifier(from, s)
		if u == "" || seen[u] || h.files[u] == nil {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// resolveSpecifier is the URL a browser fetches for specifier s in the module
// at URL from, for the two forms the trees use: an absolute path and a
// relative one. Anything else (a bare name, a full URL, a query) is "".
func resolveSpecifier(from, s string) string {
	if strings.ContainsAny(s, "?#") {
		return ""
	}
	switch {
	case strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//"):
		return path.Clean(s)
	case strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"):
		return path.Join(path.Dir(from), s)
	}
	return ""
}

// scanImports returns the module specifiers in a JavaScript source: static
// import and export-from, and import() of a single string literal.
//
// It reads tokens, not lines, so an import list across lines is read, and an
// `import(` in a comment, a string or a template literal is not. It is not a
// parser: a '/' starts a regular expression unless the token before it ends
// an expression, which is the rule every JS highlighter uses and is wrong
// only on code the trees do not contain (the real-tree pins in both www
// packages compare it with a line reader on every served file).
func scanImports(src []byte) []string {
	lx := &jsLexer{src: src}
	var specs []string
	var prev jsTok
	for {
		t := lx.next()
		if t.kind == tokEOF {
			break
		}
		if t.kind == tokIdent && !(prev.kind == tokPunct && (prev.text == "." || prev.text == "?.")) {
			switch t.text {
			case "import":
				specs = append(specs, lx.importClause()...)
			case "export":
				specs = append(specs, lx.exportClause()...)
			}
		}
		prev = t
	}
	return specs
}

// importClause reads what follows `import`.
func (lx *jsLexer) importClause() []string {
	t := lx.next()
	switch {
	case t.kind == tokString: // import 'x'
		return []string{t.text}
	case t.kind == tokPunct && t.text == "(": // import('x')
		s := lx.next()
		if s.kind != tokString {
			lx.unread(s)
			return nil
		}
		if c := lx.next(); c.kind == tokPunct && (c.text == ")" || c.text == ",") {
			return []string{s.text}
		} else {
			lx.unread(c)
		}
		return nil
	case t.kind == tokPunct && t.text == ".": // import.meta
		return nil
	}
	lx.unread(t)
	return lx.fromClause()
}

// exportClause reads what follows `export`: only `export * …` and
// `export { … }` can carry a from.
func (lx *jsLexer) exportClause() []string {
	t := lx.next()
	if t.kind != tokPunct || (t.text != "*" && t.text != "{") {
		lx.unread(t)
		return nil
	}
	lx.unread(t)
	return lx.fromClause()
}

// fromClause reads an import or export clause up to `from '…'`, stopping at
// the first token no clause holds.
func (lx *jsLexer) fromClause() []string {
	for range 4096 {
		t := lx.next()
		switch {
		case t.kind == tokIdent && t.text == "from":
			s := lx.next()
			if s.kind == tokString {
				return []string{s.text}
			}
			lx.unread(s) // `import from from 'x'`: the default binding is named from
		case t.kind == tokIdent, t.kind == tokString:
		case t.kind == tokPunct && (t.text == "{" || t.text == "}" || t.text == "," || t.text == "*"):
		default:
			lx.unread(t)
			return nil
		}
	}
	return nil
}

type tokKind uint8

const (
	tokEOF tokKind = iota
	tokIdent
	tokNumber
	tokString
	tokTemplate // a whole template literal, or the text part before ${ or after }
	tokRegex
	tokPunct
)

type jsTok struct {
	kind tokKind
	text string // an identifier, a punctuator, a string's contents
}

// jsLexer splits JavaScript into the tokens scanImports needs. Comments are
// skipped; a template literal's ${ } expressions are lexed as code.
type jsLexer struct {
	src  []byte
	i    int
	back []jsTok
	// last is the last token produced, for the regex-or-divide decision.
	last jsTok
	// tmpl holds the brace depth at each open ${, so the } that closes it
	// resumes the template literal.
	tmpl  []int
	depth int
}

func (lx *jsLexer) unread(t jsTok) { lx.back = append(lx.back, t) }

func (lx *jsLexer) next() jsTok {
	if n := len(lx.back); n > 0 {
		t := lx.back[n-1]
		lx.back = lx.back[:n-1]
		return t
	}
	t := lx.scan()
	if t.kind != tokEOF {
		lx.last = t
	}
	return t
}

func (lx *jsLexer) scan() jsTok {
	src, n := lx.src, len(lx.src)
	if lx.i == 0 && n > 1 && src[0] == '#' && src[1] == '!' {
		lx.skipLine()
	}
	for lx.i < n {
		c := src[lx.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			lx.i++
		case c == '/' && lx.i+1 < n && src[lx.i+1] == '/':
			lx.skipLine()
		case c == '/' && lx.i+1 < n && src[lx.i+1] == '*':
			if end := indexFrom(src, lx.i+2, "*/"); end < 0 {
				lx.i = n
			} else {
				lx.i = end + 2
			}
		case c == '"' || c == '\'':
			return lx.stringTok(c)
		case c == '`':
			lx.i++
			return lx.templateText()
		case c == '}' && len(lx.tmpl) > 0 && lx.tmpl[len(lx.tmpl)-1] == lx.depth:
			lx.tmpl = lx.tmpl[:len(lx.tmpl)-1]
			lx.i++
			return lx.templateText()
		case c == '/' && lx.regexAllowed():
			return lx.regexTok()
		case isJSIdentByte(c) || c == '\\':
			return lx.wordTok()
		default:
			return lx.punctTok(c)
		}
	}
	return jsTok{kind: tokEOF}
}

func (lx *jsLexer) skipLine() {
	for lx.i < len(lx.src) && lx.src[lx.i] != '\n' {
		lx.i++
	}
}

// stringTok reads a quoted string from its opening quote q; an unclosed one
// ends at the line.
func (lx *jsLexer) stringTok(q byte) jsTok {
	src, n := lx.src, len(lx.src)
	start := lx.i + 1
	j := start
	for j < n && src[j] != q && src[j] != '\n' {
		if src[j] == '\\' {
			j++
		}
		j++
	}
	lx.i = min(j+1, n)
	return jsTok{kind: tokString, text: string(src[start:min(j, n)])}
}

// regexTok reads a regular expression literal from its opening '/': a '/'
// inside a [class] does not close it.
func (lx *jsLexer) regexTok() jsTok {
	src, n := lx.src, len(lx.src)
	j := lx.i + 1
	inClass := false
	for j < n && src[j] != '\n' {
		ch := src[j]
		if ch == '\\' {
			j += 2
			continue
		}
		if inClass {
			inClass = ch != ']'
		} else if ch == '[' {
			inClass = true
		} else if ch == '/' {
			break
		}
		j++
	}
	j++
	for j < n && isJSIdentByte(src[j]) { // flags
		j++
	}
	lx.i = min(j, n)
	return jsTok{kind: tokRegex}
}

// wordTok reads an identifier, a keyword or a number.
func (lx *jsLexer) wordTok() jsTok {
	src, n := lx.src, len(lx.src)
	start := lx.i
	for lx.i < n && (isJSIdentByte(src[lx.i]) || src[lx.i] == '\\') {
		lx.i++
	}
	if c := src[start]; c >= '0' && c <= '9' {
		return jsTok{kind: tokNumber}
	}
	return jsTok{kind: tokIdent, text: string(src[start:lx.i])}
}

// punctTok reads a punctuator: as one token the few the clause readers and
// the regex-or-divide rule need whole, else one byte. Braces are counted so
// the } that closes a template's ${ is told from a block's.
func (lx *jsLexer) punctTok(c byte) jsTok {
	src, n := lx.src, len(lx.src)
	lx.i++
	switch c {
	case '{':
		lx.depth++
	case '}':
		lx.depth--
	case '?':
		if lx.i < n && src[lx.i] == '.' && !(lx.i+1 < n && src[lx.i+1] >= '0' && src[lx.i+1] <= '9') {
			lx.i++
			return jsTok{kind: tokPunct, text: "?."}
		}
	case '=':
		if lx.i < n && src[lx.i] == '>' {
			lx.i++
			return jsTok{kind: tokPunct, text: "=>"}
		}
	case '+', '-':
		if lx.i < n && src[lx.i] == c {
			lx.i++
			return jsTok{kind: tokPunct, text: string([]byte{c, c})}
		}
	}
	return jsTok{kind: tokPunct, text: string(c)}
}

// templateText reads template-literal text from just past a ` or the } of a
// ${ } to the closing ` or the next ${.
func (lx *jsLexer) templateText() jsTok {
	src, n := lx.src, len(lx.src)
	for lx.i < n {
		switch c := src[lx.i]; {
		case c == '\\':
			lx.i += 2
		case c == '`':
			lx.i++
			return jsTok{kind: tokTemplate}
		case c == '$' && lx.i+1 < n && src[lx.i+1] == '{':
			lx.i += 2
			lx.tmpl = append(lx.tmpl, lx.depth)
			return jsTok{kind: tokPunct, text: "${"}
		default:
			lx.i++
		}
	}
	return jsTok{kind: tokTemplate}
}

// regexAllowed: a '/' here starts a regular expression, because the token
// before it cannot end an expression.
func (lx *jsLexer) regexAllowed() bool {
	t := lx.last
	switch t.kind {
	case tokEOF:
		return true
	case tokNumber, tokString, tokTemplate, tokRegex:
		return false
	case tokIdent:
		switch t.text {
		case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete",
			"void", "throw", "instanceof", "yield", "await":
			return true
		}
		return false
	}
	switch t.text {
	case ")", "]":
		return false
	case "++", "--":
		// Postfix after an operand, prefix otherwise; a postfix ends an
		// expression. Unknowable from one token, and `a++ / b` is the only
		// form the trees could hold.
		return false
	}
	return true
}

func isJSIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func indexFrom(b []byte, from int, sep string) int {
	if from >= len(b) {
		return -1
	}
	if i := bytes.Index(b[from:], []byte(sep)); i >= 0 {
		return from + i
	}
	return -1
}
