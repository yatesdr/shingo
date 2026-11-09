package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// writeTree writes files (slash paths → contents) under a temp root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// githubSlug is the anchor GitHub gives a heading: lowercased, everything but
// letters, digits, spaces, hyphens and underscores dropped, spaces to hyphens.
func githubSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}

// TestWireProtocolDocHasEveryWireStruct is the coverage half of the drift
// test: the drift test proves each block is current, this proves the set of
// blocks is the set of wire structs. Every struct a message reaches (its
// payload, the envelope, and every struct their fields reference) has exactly
// one `protocol struct` block in docs/wire-protocol.md under a heading whose
// anchor the registries' and type columns' links use; and no block names a
// struct no message reaches.
func TestWireProtocolDocHasEveryWireStruct(t *testing.T) {
	want, err := wireStructs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot, "docs", "wire-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	blocks := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^<!-- generated: protocol struct (\w+) -->$`).FindAllStringSubmatch(doc, -1) {
		blocks[m[1]]++
	}
	anchors := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#+ (.+)$`).FindAllStringSubmatch(doc, -1) {
		anchors[githubSlug(m[1])] = true
	}
	live := map[string]bool{}
	for _, s := range want {
		live[s] = true
		switch n := blocks[s]; {
		case n == 0:
			t.Errorf("protocol.%s is on the wire but docs/wire-protocol.md has no `protocol struct %s` block — add a heading and the block", s, s)
		case n > 1:
			t.Errorf("protocol.%s has %d blocks in docs/wire-protocol.md, want one", s, n)
		}
		if !anchors[strings.ToLower(s)] {
			t.Errorf("no heading in docs/wire-protocol.md anchors #%s, which the generated links to protocol.%s use", strings.ToLower(s), s)
		}
	}
	for s := range blocks {
		if !live[s] {
			t.Errorf("docs/wire-protocol.md has a block for protocol.%s, which no message carries — delete its section", s)
		}
	}
}

// The fixture tree for the parse tests: a protocol package and two
// composition roots registering against it.
var wireFixture = map[string]string{
	"protocol/p.go": `package protocol

import "encoding/json"

const (
	TypeData = "data"
	TypePing = "x.ping"
	TypePong = "x.pong"
)

const (
	SubjectHello = "s.hello"
	SubjectBare  = "s.bare"
)

type Kind string

const (
	KindA Kind = "a" // the first kind
	// KindB is the second kind.
	KindB Kind = "b"
)

type Height struct{ n int }

func (h Height) MarshalText() ([]byte, error) { return nil, nil }

type Envelope struct {
	Version int ` + "`json:\"v\"`" + `
}

type Data struct {
	Subject string          ` + "`json:\"subject\"`" + `
	Body    json.RawMessage ` + "`json:\"data\"`" + `
}

type Base struct {
	// ID is the id.
	ID int64 ` + "`json:\"id\"`" + `
}

// Ping is a ping.
type Ping struct {
	Base
	// Name and Alias name it.
	Name  string ` + "`json:\"name\"`" + `
	Alias string ` + "`json:\"alias,omitempty\"`" + `
	Skip  string ` + "`json:\"-\"`" + `
	hidden string
	Kind  Kind              ` + "`json:\"kind\"`" + `
	Want  Height            ` + "`json:\"want,omitzero\"`" + `
	Items []Item            ` + "`json:\"items\"`" + `
	Tags  map[string]string ` + "`json:\"tags\"`" + `
	Count *int              ` + "`json:\"count,omitempty\"`" + ` // a count | maybe <none>
}

type Item struct {
	Code string ` + "`json:\"code\"`" + `
}

type Pong struct {
	OK bool ` + "`json:\"ok\"`" + `
}

type Hello struct {
	Who string ` + "`json:\"who\"`" + `
}
`,
	"shingo-core/cmd/shingocore/routers.go": `package main

func build() {
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { use(p) })
	router.Register(r, protocol.TypePing, h.HandlePing)
	router.Register(r, protocol.TypePong, func(*protocol.Envelope, *protocol.Pong) {})
	router.RegisterSubject(r, protocol.SubjectHello, svc.HandleHello)
	router.RegisterSubjectBare(r, protocol.SubjectBare, svc.HandleBare)
}
`,
	"shingo-core/messaging/h.go": `package messaging

func (h *Handler) HandlePing(env *protocol.Envelope, p *protocol.Ping) {}
func (s *Service) HandleHello(env *protocol.Envelope, p *protocol.Hello) {}
func (s *Service) HandleBare(env *protocol.Envelope) {}
`,
	"shingo-edge/cmd/shingoedge/main.go": `package main

func main() {
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { use(p) })
	router.Register(r, protocol.TypePing, func(*protocol.Envelope, *protocol.Ping) {})
	router.Register(r, protocol.TypePong, edge.HandlePong)
}
`,
	"shingo-edge/messaging/h.go": `package messaging

func (h *EdgeHandler) HandlePong(env *protocol.Envelope, p *protocol.Pong) {}
`,
}

// TestReadRegistrations_ResolvesEachHandlerShape pins the composition-root
// parse: a func literal names its struct, a method value resolves through its
// module, a bare subject carries none, and an empty literal is a side that
// drops the type.
func TestReadRegistrations_ResolvesEachHandlerShape(t *testing.T) {
	root := writeTree(t, wireFixture)
	core, err := readRegistrations(root, coreRoutersFile, "shingo-core")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := readRegistrations(root, edgeMainFile, "shingo-edge")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		side     map[string]registration
		key      string
		payload  string
		acts     bool
		sideName string
	}{
		{core, "TypeData", "Data", true, "core"},
		{core, "TypePing", "Ping", true, "core"},
		{core, "TypePong", "Pong", false, "core"},
		{core, "SubjectHello", "Hello", true, "core"},
		{core, "SubjectBare", "", true, "core"},
		{edge, "TypePing", "Ping", false, "edge"},
		{edge, "TypePong", "Pong", true, "edge"},
	}
	for _, c := range cases {
		got, ok := c.side[c.key]
		if !ok {
			t.Errorf("%s: %s not registered", c.sideName, c.key)
			continue
		}
		if got.payload != c.payload || got.acts != c.acts {
			t.Errorf("%s %s = {payload %q, acts %v}, want {%q, %v}", c.sideName, c.key, got.payload, got.acts, c.payload, c.acts)
		}
	}
}

// TestReadRegistrations_Refuses: a method name that resolves to two payload
// structs in its module is an error, never a guess.
func TestReadRegistrations_Refuses(t *testing.T) {
	files := map[string]string{}
	for k, v := range wireFixture {
		files[k] = v
	}
	files["shingo-core/other/h.go"] = "package other\n\nfunc (x *X) HandlePing(env *protocol.Envelope, p *protocol.Pong) {}\n"
	root := writeTree(t, files)
	if _, err := readRegistrations(root, coreRoutersFile, "shingo-core"); err == nil || !strings.Contains(err.Error(), "HandlePing") {
		t.Fatalf("an ambiguous handler parsed: %v", err)
	}
}

// TestStructFields_ReadsTheJSONShape pins the field-table parse: embedded
// fields flatten, `-` and unexported fields drop, omitempty/omitzero show, a
// shared doc comment reaches the field it names, named and custom-text types
// render as their JSON shape, and a referenced struct is listed for its own
// table.
func TestStructFields_ReadsTheJSONShape(t *testing.T) {
	root := writeTree(t, wireFixture)
	p, err := loadProto(root)
	if err != nil {
		t.Fatal(err)
	}
	fields, refs, err := p.structFields("Ping")
	if err != nil {
		t.Fatal(err)
	}
	want := []wireField{
		{key: "id", typ: "integer", desc: "ID is the id."},
		{key: "name", typ: "string", desc: "Name and Alias name it."},
		{key: "alias", typ: "string", omit: "omitempty", desc: "See `name`."},
		{key: "kind", typ: "string (`Kind`)"},
		{key: "want", typ: "string (`Height`)", omit: "omitzero"},
		{key: "items", typ: "[`Item`](#item)[]"},
		{key: "tags", typ: "object (string → string)"},
		{key: "count", typ: "integer", omit: "omitempty", desc: "a count | maybe <none>"},
	}
	if len(fields) != len(want) {
		t.Fatalf("fields = %+v\nwant %+v", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, fields[i], want[i])
		}
	}
	if len(refs) != 1 || refs[0] != "Item" {
		t.Errorf("refs = %v, want [Item]", refs)
	}
	body, err := emitStruct(root, []string{"Ping"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `| a count \| maybe &lt;none> |`) {
		t.Errorf("a description's pipe and angle bracket are not escaped:\n%s", body)
	}
	if _, err := emitStruct(root, []string{"Nope"}); err == nil {
		t.Error("a struct protocol does not declare emitted without error")
	}
}

// TestMessages_DirectionAndPayload pins the registries' rows on the fixture:
// a type is Edge → Core when only Core's handler acts, Core → Edge when only
// the Edge's does, both for data; the payload is the decoded struct.
func TestMessages_DirectionAndPayload(t *testing.T) {
	root := writeTree(t, wireFixture)
	r, err := loadRoots(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := func(core, edge registration) string {
		switch {
		case core.acts && edge.acts:
			return dirBoth
		case core.acts:
			return dirEdgeToCore
		}
		return dirCoreToEdge
	}
	got := map[string]string{}
	for _, k := range []string{"TypeData", "TypePing", "TypePong"} {
		got[k] = dir(r.core[k], r.edge[k])
	}
	want := map[string]string{"TypeData": dirBoth, "TypePing": dirEdgeToCore, "TypePong": dirCoreToEdge}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s direction = %q, want %q", k, got[k], w)
		}
	}
}

// TestWireStructs_RealTree: on the real tree every message reaches a struct
// and the walk ends at structs, so a renamed payload or a field of an
// unknown struct type fails here before it reaches a doc.
func TestWireStructs_RealTree(t *testing.T) {
	ss, err := wireStructs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !sort.StringsAreSorted(ss) {
		t.Error("wireStructs is not sorted")
	}
	for _, must := range []string{"Envelope", "RawHeader", "Address", "Data", "OrderRequest", "EdgeHeartbeatAck", "NodeBinsRow"} {
		if i := sort.SearchStrings(ss, must); i == len(ss) || ss[i] != must {
			t.Errorf("wireStructs misses %s", must)
		}
	}
}
