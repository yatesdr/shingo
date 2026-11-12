package assets

import (
	"encoding/json"
	"html/template"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// What scanImports reads out of a module, and what it must not read: an
// import in a comment, a string, a template literal's text or a regular
// expression, a member call named import, import.meta, a computed import().
func TestScanImports(t *testing.T) {
	for _, c := range []struct {
		name string
		src  string
		want []string
	}{
		{"named", `import { a } from './a.js';`, []string{"./a.js"}},
		{"default, double quotes", `import b from "/static/b.js"`, []string{"/static/b.js"}},
		{"namespace", `import * as c from '../c.js'`, []string{"../c.js"}},
		{"default and named", `import x, { y as z } from './xy.js'`, []string{"./xy.js"}},
		{"side effect", `import './side.js';`, []string{"./side.js"}},
		{"across lines", "import {\n  a,\n  b, // the b\n  c,\n} from './multi.js';", []string{"./multi.js"}},
		{"export named from", `export { x, y as z } from './x.js'`, []string{"./x.js"}},
		{"export star from", `export * from './star.js'`, []string{"./star.js"}},
		{"export star as from", `export * as ns from './ns.js'`, []string{"./ns.js"}},
		{"export a local list has no from", `const q = 1; export { q }; import './after.js'`, []string{"./after.js"}},
		{"export declarations have no from", "export const a = 1;\nexport default function f() {}\nexport function g() {}", nil},
		{"dynamic", `const m = await import('./lazy.js');`, []string{"./lazy.js"}},
		{"dynamic with options", `import("./opt.js", { with: {} })`, []string{"./opt.js"}},
		{"dynamic inside a template expression is code", "const s = `${await import('./intpl.js')}`;", []string{"./intpl.js"}},
		{"several in order", "import './1.js';\nexport * from './2.js';\nimport('./3.js');", []string{"./1.js", "./2.js", "./3.js"}},

		{"line comment", "// import './commented.js'\n", nil},
		{"block comment", "/* import x from './c.js'; import('./d.js') */", nil},
		{"string", `const s = "import './str.js'"; const u = 'export * from "./u.js"';`, nil},
		{"template text", "const s = `import('./tpl.js') ${1} import './tpl2.js'`;", nil},
		{"regular expression", `const re = /import '.\/re.js'/g;`, nil},
		{"regular expression with a slash in a class", `const re = /[/]import('.\/re.js')/;`, nil},
		{"division is not a regular expression", "const r = a / b; import './after-div.js'; const s = c / d;", []string{"./after-div.js"}},
		{"member named import", `loader.import('./member.js'); x?.import('./opt-member.js')`, nil},
		{"import.meta", `const u = new URL('./meta.js', import.meta.url);`, nil},
		{"computed import", `import(base + '/x.js'); import(name)`, nil},
		{"hashbang", "#!/usr/bin/env node\nimport './hb.js'", []string{"./hb.js"}},
	} {
		got := scanImports([]byte(c.src))
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n  src  %s\n  got  %q\n  want %q", c.name, c.src, got, c.want)
		}
	}
}

// ScanModuleImports splits what scanImports reads: an import() is fetched when
// it runs, so a page's boot does not count it.
func TestScanModuleImports_SplitsStaticFromDynamic(t *testing.T) {
	src := "import './1.js';\nexport * from './2.js';\nimport('./3.js');\nimport x from './4.js';"
	static, dynamic := ScanModuleImports([]byte(src))
	if want := []string{"./1.js", "./2.js", "./4.js"}; !reflect.DeepEqual(static, want) {
		t.Errorf("static %q, want %q", static, want)
	}
	if want := []string{"./3.js"}; !reflect.DeepEqual(dynamic, want) {
		t.Errorf("dynamic %q, want %q", dynamic, want)
	}
}

func TestResolveSpecifier(t *testing.T) {
	for _, c := range []struct{ from, spec, want string }{
		{"/static/a/x.js", "./y.js", "/static/a/y.js"},
		{"/static/a/x.js", "../shared/u.js", "/static/shared/u.js"},
		{"/static/a/x.js", "/static/shared/u.js", "/static/shared/u.js"},
		{"/static/a/x.js", "/static/a/../b.js", "/static/b.js"},
		{"/static/a/x.js", "lodash", ""},
		{"/static/a/x.js", "https://cdn.example/x.js", ""},
		{"/static/a/x.js", "//cdn.example/x.js", ""},
		{"/static/a/x.js", "./y.js?v=1", ""},
	} {
		if got := resolveSpecifier(c.from, c.spec); got != c.want {
			t.Errorf("resolveSpecifier(%s, %s) = %q, want %q", c.from, c.spec, got, c.want)
		}
	}
}

// A page tree: an entry that reaches modules by a static import, an
// export-from, a dynamic import and an absolute import into the shared mount,
// and modules no entry reaches.
func importTree() (fstest.MapFS, fstest.MapFS) {
	static := fstest.MapFS{
		"pages/entry.js":     {Data: []byte("import { a } from './a.js';\nimport '/static/shared/util.js';\nconst lazy = () => import('../lazy/l.js');\n")},
		"pages/a.js":         {Data: []byte("export * from './b.js';\nexport { c } from '../lib/c.js';\nimport 'https://cdn.example/x.js';\nimport 'bare';\n")},
		"pages/b.js":         {Data: []byte("export const b = 1;\n")},
		"lib/c.js":           {Data: []byte("import { util } from '../shared/util.js';\nexport const c = util;\n")},
		"lazy/l.js":          {Data: []byte("import './missing.js';\nexport default 1;\n")},
		"pages/other.js":     {Data: []byte("import './unreached.js';\n")},
		"pages/unreached.js": {Data: []byte("export {};\n")},
		"pages/a.test.js":    {Data: []byte("import './a.js';\n")},
		"pages/style.css":    {Data: []byte("a{}\n")},
	}
	shared := fstest.MapFS{
		"util.js":      {Data: []byte("import './tokens.js';\nexport function util() {}\n")},
		"tokens.js":    {Data: []byte("export const t = 1;\n")},
		"util.test.js": {Data: []byte("import './util.js';\n")},
	}
	return static, shared
}

// The map holds every module the entry reaches — through a static import, an
// export-from, a dynamic import, across mounts by an absolute or a relative
// URL — and nothing else; every key is a plain URL the handler serves, every
// value its hashed URL.
func TestImportMapCoversEveryReachableModule(t *testing.T) {
	static, shared := importTree()
	h, err := New(Mount{"/static/", static}, Mount{"/static/shared/", shared})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/static/lazy/l.js",
		"/static/lib/c.js",
		"/static/pages/a.js",
		"/static/pages/b.js",
		"/static/pages/entry.js",
		"/static/shared/tokens.js",
		"/static/shared/util.js",
	}
	for _, entry := range []string{"/static/pages/entry.js", h.URL("/static/pages/entry.js")} {
		m := h.ImportMap(entry)
		var keys []string
		for k, v := range m {
			keys = append(keys, k)
			if _, _, hashed := splitHashed(k); hashed || h.files[k] == nil {
				t.Errorf("entry %s: key %s is not a plain URL the handler serves", entry, k)
			}
			if v != h.URL(k) || v == k {
				t.Errorf("entry %s: %s maps to %s, want its hashed URL %s", entry, k, v, h.URL(k))
			}
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, want) {
			t.Errorf("entry %s: keys\n  %q\nwant\n  %q", entry, keys, want)
		}
	}
	if m := h.ImportMap("/static/nope.js"); len(m) != 0 {
		t.Errorf("an entry the handler does not serve gave %v", m)
	}
	if m := h.ImportMap("/static/pages/b.js", "/static/pages/other.js"); len(m) != 3 {
		t.Errorf("two entries: %v, want b, other and unreached", m)
	}
}

// The tag is one <script type="importmap"> whose JSON is the map, its keys
// sorted, with no character that could close the element early; built once
// per entry list.
func TestImportMapTag(t *testing.T) {
	static, shared := importTree()
	static["pages/x<y>.js"] = &fstest.MapFile{Data: []byte("export {};\n")}
	static["pages/entry.js"] = &fstest.MapFile{Data: []byte("import './x<y>.js';\n")}
	h, err := New(Mount{"/static/", static}, Mount{"/static/shared/", shared})
	if err != nil {
		t.Fatal(err)
	}
	tag := string(h.ImportMapTag("/static/pages/entry.js"))
	const open, end = `<script type="importmap">`, `</script>`
	if !strings.HasPrefix(tag, open) || !strings.HasSuffix(tag, end) || strings.Count(tag, "<") != 2 {
		t.Fatalf("tag %s", tag)
	}
	var doc struct {
		Imports map[string]string `json:"imports"`
	}
	body := strings.TrimSuffix(strings.TrimPrefix(tag, open), end)
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !reflect.DeepEqual(doc.Imports, h.ImportMap("/static/pages/entry.js")) {
		t.Errorf("tag %v, map %v", doc.Imports, h.ImportMap("/static/pages/entry.js"))
	}
	if again := string(h.ImportMapTag("/static/pages/entry.js")); again != tag {
		t.Errorf("a second call gave a different tag")
	}
}

// The two names both routers' FuncMaps register.
func TestFuncMap(t *testing.T) {
	h := newTestHandler(t)
	fm := h.FuncMap()
	if len(fm) != 2 {
		t.Errorf("FuncMap has %d functions, want assetURL and importMap", len(fm))
	}
	url, ok := fm["assetURL"].(func(string) string)
	if !ok || url("/static/app.js") != h.URL("/static/app.js") {
		t.Errorf("assetURL is %T", fm["assetURL"])
	}
	if _, ok := fm["importMap"].(func(...string) template.HTML); !ok {
		t.Errorf("importMap is %T", fm["importMap"])
	}
}
