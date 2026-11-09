package main

// wire.go — the generated blocks of docs/wire-protocol.md: the message type
// and subject registries, their TTLs, every wire struct's field table, and the
// order status table.
//
// TWO KINDS OF SOURCE. The lists and values come from the protocol package
// itself, imported (AllTypes, AllSubjects, CoreInboundSubjects, DefaultTTLFor,
// DataTTLFor, AllStatuses, AllValidTransitions): they are the running truth,
// not a reading of it. What only the source text carries — json tags, field
// doc comments, constant names and comments — is read from protocol/*.go by
// go/parser, relative to root.
//
// WHICH STRUCT A MESSAGE CARRIES is said nowhere in the protocol package: it
// is the type parameter of each handler registration in the two composition
// roots (router.Register / router.RegisterSubject in shingo-core's routers.go
// and shingo-edge's main.go). Those are parsed too: a func literal names the
// struct in its second parameter, a method value is resolved to the method's
// declaration in its module. A type's direction is which side registers a
// handler that does something; the other side's is an empty func literal.

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
)

func init() {
	register(Generator{Name: "protocol/types.go AllTypes", Emit: emitTypeRegistry})
	register(Generator{Name: "protocol/types.go AllSubjects", Emit: emitSubjectRegistry})
	register(Generator{Name: "protocol/expiry.go DefaultTTLFor", Emit: emitTypeTTLs})
	register(Generator{Name: "protocol/expiry.go DataTTLFor", Emit: emitSubjectTTLs})
	register(Generator{Name: "protocol struct", Emit: emitStruct})
	register(Generator{Name: "protocol/status.go AllStatuses", Emit: emitStatuses})
}

// The composition roots whose registrations say which struct each message
// carries.
const (
	coreRoutersFile = "shingo-core/cmd/shingocore/routers.go"
	edgeMainFile    = "shingo-edge/cmd/shingoedge/main.go"
)

// ---------------------------------------------------------------------------
// The protocol package, read as source.

// protoConst is one constant of the protocol package.
type protoConst struct {
	name, value, typ string // value is the unquoted string literal ("" for non-strings)
	doc              string
}

// protoPkg is protocol/*.go (tests excluded), parsed.
type protoPkg struct {
	types   map[string]*ast.TypeSpec
	methods map[string]map[string]bool // type name → its method names
	consts  []protoConst               // in source order, file by file
}

var protoCache sync.Map // root → *protoPkg

func loadProto(root string) (*protoPkg, error) {
	if p, ok := protoCache.Load(root); ok {
		return p.(*protoPkg), nil
	}
	dir := filepath.Join(root, "protocol")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	p := &protoPkg{types: map[string]*ast.TypeSpec{}, methods: map[string]map[string]bool{}}
	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				p.addGenDecl(d)
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) == 1 {
					t := recvTypeName(d.Recv.List[0].Type)
					if p.methods[t] == nil {
						p.methods[t] = map[string]bool{}
					}
					p.methods[t][d.Name.Name] = true
				}
			}
		}
	}
	protoCache.Store(root, p)
	return p, nil
}

func (p *protoPkg) addGenDecl(d *ast.GenDecl) {
	for _, s := range d.Specs {
		switch s := s.(type) {
		case *ast.TypeSpec:
			p.types[s.Name.Name] = s
		case *ast.ValueSpec:
			if d.Tok != token.CONST {
				continue
			}
			doc := commentText(s.Doc)
			if doc == "" {
				doc = commentText(s.Comment)
			}
			if doc == "" && len(d.Specs) == 1 {
				doc = commentText(d.Doc)
			}
			typ := ""
			if id, ok := s.Type.(*ast.Ident); ok {
				typ = id.Name
			}
			for i, n := range s.Names {
				c := protoConst{name: n.Name, typ: typ, doc: doc}
				if i < len(s.Values) {
					if lit, ok := s.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						c.value, _ = strconv.Unquote(lit.Value)
					}
				}
				p.consts = append(p.consts, c)
			}
		}
	}
}

func recvTypeName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// constFor is the constant whose name has the prefix and whose value is v.
func (p *protoPkg) constFor(prefix, v string) (protoConst, bool) {
	for _, c := range p.consts {
		if c.value == v && strings.HasPrefix(c.name, prefix) {
			return c, true
		}
	}
	return protoConst{}, false
}

// commentText is a comment group as one line: the text, whitespace collapsed.
func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	return strings.Join(strings.Fields(g.Text()), " ")
}

// ---------------------------------------------------------------------------
// The composition roots.

// registration is one handler registration in a composition root.
type registration struct {
	key     string // the protocol constant registered (TypeX / SubjectX)
	payload string // the protocol struct the handler decodes; "" for a bare subject
	acts    bool   // false for an empty func literal: the side receives and drops it
}

type roots struct {
	core, edge map[string]registration
}

var rootsCache sync.Map // root → *roots

func loadRoots(root string) (*roots, error) {
	if r, ok := rootsCache.Load(root); ok {
		return r.(*roots), nil
	}
	core, err := readRegistrations(root, coreRoutersFile, "shingo-core")
	if err != nil {
		return nil, err
	}
	edge, err := readRegistrations(root, edgeMainFile, "shingo-edge")
	if err != nil {
		return nil, err
	}
	r := &roots{core: core, edge: edge}
	rootsCache.Store(root, r)
	return r, nil
}

// readRegistrations reads every router.Register / RegisterSubject /
// RegisterSubjectBare call in file, resolving method-value handlers against
// the methods of module.
func readRegistrations(root, file, module string) (map[string]registration, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(file)), nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var methods map[string]map[string]bool
	out := map[string]registration{}
	var walkErr error
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || walkErr != nil {
			return walkErr == nil
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isIdent(fn.X, "router") || len(call.Args) != 3 {
			return true
		}
		verb := fn.Sel.Name
		if verb != "Register" && verb != "RegisterSubject" && verb != "RegisterSubjectBare" {
			return true
		}
		key, ok := protocolSel(call.Args[1])
		if !ok {
			return true
		}
		at := fset.Position(call.Pos())
		reg := registration{key: key, acts: true}
		switch h := call.Args[2].(type) {
		case *ast.FuncLit:
			reg.acts = len(h.Body.List) > 0
			if verb != "RegisterSubjectBare" {
				reg.payload, ok = handlerPayload(h.Type)
				if !ok {
					walkErr = fmt.Errorf("%s:%d: the handler for %s takes no *protocol struct", file, at.Line, key)
				}
			}
		case *ast.SelectorExpr:
			if verb == "RegisterSubjectBare" {
				break
			}
			if methods == nil {
				if methods, err = handlerMethods(root, module); err != nil {
					walkErr = err
					return false
				}
			}
			set := methods[h.Sel.Name]
			if len(set) != 1 {
				walkErr = fmt.Errorf("%s:%d: the handler %s for %s resolves to %d payload structs in %s, want 1",
					file, at.Line, h.Sel.Name, key, len(set), module)
				return false
			}
			for t := range set {
				reg.payload = t
			}
		default:
			walkErr = fmt.Errorf("%s:%d: the handler for %s is neither a func literal nor a method value", file, at.Line, key)
		}
		if _, dup := out[key]; dup {
			walkErr = fmt.Errorf("%s:%d: %s is registered twice", file, at.Line, key)
		}
		out[key] = reg
		return true
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no router registrations found", file)
	}
	return out, nil
}

// handlerMethods indexes the module's methods shaped like a handler —
// (*protocol.Envelope, *protocol.T) — by method name, to the set of T.
func handlerMethods(root, module string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	marker := []byte("protocol.Envelope")
	base := filepath.Join(root, module)
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n := d.Name()
		if d.IsDir() {
			if path != base && (strings.HasPrefix(n, ".") || n == "testdata" || n == "vendor" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(src, marker) {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil {
				continue
			}
			if t, ok := handlerPayload(fd.Type); ok {
				if out[fd.Name.Name] == nil {
					out[fd.Name.Name] = map[string]bool{}
				}
				out[fd.Name.Name][t] = true
			}
		}
		return nil
	})
	return out, err
}

// handlerPayload is T for a func type (*protocol.Envelope, *protocol.T).
func handlerPayload(ft *ast.FuncType) (string, bool) {
	var params []ast.Expr
	for _, f := range ft.Params.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			params = append(params, f.Type)
		}
	}
	if len(params) != 2 {
		return "", false
	}
	if env, ok := protocolPtr(params[0]); !ok || env != "Envelope" {
		return "", false
	}
	return protocolPtr(params[1])
}

func protocolPtr(e ast.Expr) (string, bool) {
	s, ok := e.(*ast.StarExpr)
	if !ok {
		return "", false
	}
	return protocolSel(s.X)
}

func protocolSel(e ast.Expr) (string, bool) {
	s, ok := e.(*ast.SelectorExpr)
	if !ok || !isIdent(s.X, "protocol") {
		return "", false
	}
	return s.Sel.Name, true
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// ---------------------------------------------------------------------------
// The messages: every type and subject, with its direction and payload.

// message is one envelope type or data subject.
type message struct {
	wire, constName, direction, payload string
}

const (
	dirEdgeToCore = "Edge → Core"
	dirCoreToEdge = "Core → Edge"
	dirBoth       = "both"
)

// typeMessages is every protocol.AllTypes entry, in its order.
func typeMessages(root string) ([]message, error) {
	p, err := loadProto(root)
	if err != nil {
		return nil, err
	}
	r, err := loadRoots(root)
	if err != nil {
		return nil, err
	}
	var out []message
	for _, wire := range protocol.AllTypes() {
		c, ok := p.constFor("Type", wire)
		if !ok {
			return nil, fmt.Errorf("no Type constant holds %q", wire)
		}
		core, cok := r.core[c.name]
		edge, eok := r.edge[c.name]
		if !cok || !eok {
			return nil, fmt.Errorf("%s is not registered at both composition roots (core %v, edge %v)", c.name, cok, eok)
		}
		if core.payload != edge.payload {
			return nil, fmt.Errorf("%s decodes as protocol.%s at Core and protocol.%s at the Edge", c.name, core.payload, edge.payload)
		}
		m := message{wire: wire, constName: c.name, payload: core.payload}
		switch {
		case core.acts && edge.acts:
			m.direction = dirBoth
		case core.acts:
			m.direction = dirEdgeToCore
		case edge.acts:
			m.direction = dirCoreToEdge
		default:
			return nil, fmt.Errorf("%s: neither composition root handles it", c.name)
		}
		out = append(out, m)
	}
	return out, nil
}

// subjectMessages is every protocol.AllSubjects entry, sorted by wire name.
func subjectMessages(root string) ([]message, error) {
	p, err := loadProto(root)
	if err != nil {
		return nil, err
	}
	r, err := loadRoots(root)
	if err != nil {
		return nil, err
	}
	coreIn := map[string]bool{}
	for _, s := range protocol.CoreInboundSubjects() {
		coreIn[s] = true
	}
	var out []message
	for _, wire := range protocol.AllSubjects() {
		c, ok := p.constFor("Subject", wire)
		if !ok {
			return nil, fmt.Errorf("no Subject constant holds %q", wire)
		}
		m := message{wire: wire, constName: c.name, direction: dirCoreToEdge}
		side, sideName := r.edge, "the Edge"
		if coreIn[wire] {
			m.direction, side, sideName = dirEdgeToCore, r.core, "Core"
		}
		reg, ok := side[c.name]
		if !ok {
			return nil, fmt.Errorf("%s is inbound to %s but its composition root registers no handler", c.name, sideName)
		}
		m.payload = reg.payload
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].wire < out[j].wire })
	return out, nil
}

// payloadLink is a message's payload cell: a link to the struct's section.
func payloadLink(m message) string {
	if m.payload == "" {
		return "none (empty body)"
	}
	return fmt.Sprintf("[`%s`](#%s)", m.payload, strings.ToLower(m.payload))
}

func emitTypeRegistry(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	ms, err := typeMessages(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Type | Constant | Direction | Payload |\n|---|---|---|---|\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", m.wire, m.constName, m.direction, payloadLink(m))
	}
	return b.String(), nil
}

func emitSubjectRegistry(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	ms, err := subjectMessages(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Subject | Constant | Direction | Payload |\n|---|---|---|---|\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", m.wire, m.constName, m.direction, payloadLink(m))
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// TTLs.

// ttlText renders a TTL; NoExpiry is no `exp` at all.
func ttlText(d time.Duration) string {
	switch {
	case d == protocol.NoExpiry:
		return "**none**"
	case d%time.Minute == 0:
		return fmt.Sprintf("%d min", d/time.Minute)
	default:
		return fmt.Sprintf("%d s", d/time.Second)
	}
}

func emitTypeTTLs(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	ms, err := typeMessages(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Type | TTL |\n|---|---|\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "| `%s` | %s |\n", m.wire, ttlText(protocol.DefaultTTLFor(m.wire)))
	}
	fmt.Fprintf(&b, "\nA type not listed gets `FallbackTTL` (%s). A `data` envelope's TTL is its subject's (below).\n",
		ttlText(protocol.FallbackTTL))
	return b.String(), nil
}

func emitSubjectTTLs(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	ms, err := subjectMessages(root)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Subject | TTL |\n|---|---|\n")
	for _, m := range ms {
		fmt.Fprintf(&b, "| `%s` | %s |\n", m.wire, ttlText(protocol.DataTTLFor(m.wire)))
	}
	fmt.Fprintf(&b, "\nA subject not listed gets the `data` type's TTL (%s).\n",
		ttlText(protocol.DefaultTTLFor(protocol.TypeData)))
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// Struct field tables.

// wireField is one JSON key of a wire struct.
type wireField struct {
	key, typ, omit, desc string
}

// structFields reads a protocol struct's JSON fields in declaration order,
// flattening embedded structs the way encoding/json does, and returns the
// protocol structs its fields reference.
func (p *protoPkg) structFields(name string) ([]wireField, []string, error) {
	spec, ok := p.types[name]
	if !ok {
		return nil, nil, fmt.Errorf("protocol declares no type %s", name)
	}
	st, ok := spec.Type.(*ast.StructType)
	if !ok {
		return nil, nil, fmt.Errorf("protocol.%s is not a struct", name)
	}
	var out []wireField
	var refs []string
	type documented struct{ text, key string }
	var prev documented
	for _, f := range st.Fields.List {
		tag := ""
		if f.Tag != nil {
			raw, _ := strconv.Unquote(f.Tag.Value)
			tag = reflect.StructTag(raw).Get("json")
		}
		jsonName, opts, _ := strings.Cut(tag, ",")
		if jsonName == "-" && opts == "" {
			continue
		}
		if len(f.Names) == 0 { // embedded
			if jsonName == "" {
				emb := typeName(f.Type)
				sub, subRefs, err := p.structFields(emb)
				if err != nil {
					return nil, nil, fmt.Errorf("%s embeds %s: %w", name, emb, err)
				}
				out = append(out, sub...)
				refs = append(refs, subRefs...)
				continue
			}
			f.Names = []*ast.Ident{ast.NewIdent(typeName(f.Type))}
		}
		doc := commentText(f.Doc)
		if doc == "" {
			doc = commentText(f.Comment)
		}
		typ, nested := p.wireType(f.Type)
		refs = append(refs, nested...)
		omit := ""
		for _, o := range strings.Split(opts, ",") {
			if o == "omitempty" || o == "omitzero" {
				omit = o
			}
		}
		for _, n := range f.Names {
			if !ast.IsExported(n.Name) {
				continue
			}
			key := jsonName
			if key == "" {
				key = n.Name
			}
			desc := doc
			if desc == "" && prev.text != "" && mentions(prev.text, n.Name) {
				desc = fmt.Sprintf("See `%s`.", prev.key)
			}
			if doc != "" {
				prev = documented{doc, key}
			}
			out = append(out, wireField{key: key, typ: typ, omit: omit, desc: desc})
		}
	}
	return out, refs, nil
}

func mentions(text, ident string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(ident) + `\b`).MatchString(text)
}

func typeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return typeName(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

var basicWire = map[string]string{
	"string": "string", "bool": "boolean", "byte": "integer", "rune": "integer",
	"int": "integer", "int8": "integer", "int16": "integer", "int32": "integer", "int64": "integer",
	"uint": "integer", "uint8": "integer", "uint16": "integer", "uint32": "integer", "uint64": "integer",
	"float32": "float", "float64": "float", "any": "any JSON",
}

// wireType renders a field's Go type as its JSON shape, and lists the
// protocol structs it reaches (each gets its own table).
func (p *protoPkg) wireType(e ast.Expr) (string, []string) {
	switch e := e.(type) {
	case *ast.StarExpr:
		return p.wireType(e.X)
	case *ast.ArrayType:
		if isIdent(e.Elt, "byte") {
			return "string (base64)", nil
		}
		t, refs := p.wireType(e.Elt)
		return t + "[]", refs
	case *ast.MapType:
		k, _ := p.wireType(e.Key)
		v, refs := p.wireType(e.Value)
		return "object (" + k + " → " + v + ")", refs
	case *ast.InterfaceType:
		return "any JSON", nil
	case *ast.SelectorExpr:
		switch typeName(e.X) + "." + e.Sel.Name {
		case "time.Time":
			return "timestamp", nil
		case "time.Duration":
			return "integer (ns)", nil
		case "json.RawMessage":
			return "JSON", nil
		}
		return "`" + typeName(e.X) + "." + e.Sel.Name + "`", nil
	case *ast.Ident:
		if w, ok := basicWire[e.Name]; ok {
			return w, nil
		}
		spec, ok := p.types[e.Name]
		if !ok {
			return "`" + e.Name + "`", nil
		}
		m := p.methods[e.Name]
		switch {
		case m["MarshalJSON"]:
			return "`" + e.Name + "` (its own JSON)", nil
		case m["MarshalText"]:
			return "string (`" + e.Name + "`)", nil
		}
		if _, ok := spec.Type.(*ast.StructType); ok {
			return fmt.Sprintf("[`%s`](#%s)", e.Name, strings.ToLower(e.Name)), []string{e.Name}
		}
		under, refs := p.wireType(spec.Type)
		return under + " (`" + e.Name + "`)", refs
	}
	return "", nil
}

// cell makes text safe inside a markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "<", "&lt;")
	return s
}

func emitStruct(root string, args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("takes one argument, the struct's name")
	}
	p, err := loadProto(root)
	if err != nil {
		return "", err
	}
	fields, _, err := p.structFields(args[0])
	if err != nil {
		return "", err
	}
	if len(fields) == 0 {
		return "An empty object: `{}`.\n", nil
	}
	var b strings.Builder
	b.WriteString("| Field | Type | Omitted when zero | Description |\n|---|---|---|---|\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", f.key, f.typ, f.omit, cell(f.desc))
	}
	return b.String(), nil
}

// wireStructs is every protocol struct a message reaches: each message's
// payload, the envelope, and every struct their fields reference, sorted.
func wireStructs(root string) ([]string, error) {
	p, err := loadProto(root)
	if err != nil {
		return nil, err
	}
	types, err := typeMessages(root)
	if err != nil {
		return nil, err
	}
	subjects, err := subjectMessages(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	queue := []string{"Envelope", "RawHeader"}
	for _, m := range append(types, subjects...) {
		if m.payload != "" {
			queue = append(queue, m.payload)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n] {
			continue
		}
		seen[n] = true
		_, refs, err := p.structFields(n)
		if err != nil {
			return nil, err
		}
		queue = append(queue, refs...)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// Order statuses.

func emitStatuses(root string, args []string) (string, error) {
	if len(args) > 0 {
		return "", fmt.Errorf("takes no arguments")
	}
	p, err := loadProto(root)
	if err != nil {
		return "", err
	}
	next := protocol.AllValidTransitions()
	var b strings.Builder
	b.WriteString("| Status | Constant | Terminal | Moves to |\n|---|---|---|---|\n")
	for _, s := range protocol.AllStatuses() {
		c, ok := p.constFor("Status", string(s))
		if !ok {
			return "", fmt.Errorf("no Status constant holds %q", s)
		}
		terminal := "no"
		if protocol.IsTerminal(s) {
			terminal = "**yes**"
		}
		var to []string
		for _, t := range next[s] {
			to = append(to, "`"+string(t)+"`")
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", s, c.name, terminal, strings.Join(to, ", "))
	}
	return b.String(), nil
}
