package assetcheck

import (
	"html/template"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"shingo/shared/assets"
)

func testHandler(t *testing.T) *assets.Handler {
	t.Helper()
	pad := strings.Repeat("// pad\n", 60)
	h, err := assets.New(assets.Mount{Prefix: "/static/", FS: fstest.MapFS{
		"hub.js":      {Data: []byte("import { d } from './dep.js';\n" + pad)},
		"page.js":     {Data: []byte("import '/static/hub.js';\nconst lazy = () => import('./lazy.js');\n" + pad)},
		"dep.js":      {Data: []byte("export const d = 1;\n" + pad)},
		"lazy.js":     {Data: []byte("import { d } from './dep.js';\nimport './deep.js';\n" + pad)},
		"deep.js":     {Data: []byte("export {};\n")},
		"a.css":       {Data: []byte(".a { color: red; }\n")},
		"c.js":        {Data: []byte("var c = 1;\n")},
		"t.test.js":   {Data: []byte("// a test\n")},
		"bad.js":      {Data: []byte("import './t.test.js';\n")},
		"favicon.ico": {Data: []byte("\x00\x00\x01\x00icon")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// page is a page the way the templates write one: hashed tags, the map
// first, the entries as inline imports.
func page(h *assets.Handler, entries ...string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><link rel="icon" href="` + h.URL("/static/favicon.ico") + `">`)
	b.WriteString(`<link rel="stylesheet" href="` + h.URL("/static/a.css") + `">`)
	b.WriteString(string(h.ImportMapTag(entries...)))
	b.WriteString(`</head><body><script src="` + h.URL("/static/c.js") + `"></script>`)
	for _, e := range entries {
		b.WriteString(`<script type="module">import "` + e + `";</script>`)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

func noProblems(t *testing.T, label string, r Result) {
	t.Helper()
	for _, p := range r.Problems {
		t.Errorf("%s: %s", label, p)
	}
}

func TestLoad_BothBrowsersLoadOneURLPerModule(t *testing.T) {
	h := testHandler(t)
	html := page(h, "/static/hub.js", "/static/page.js")
	with := Load(h, html, WithImportMap)
	without := Load(h, html, NoImportMap)
	noProblems(t, "with a map", with)
	noProblems(t, "without", without)
	want := []string{"/static/deep.js", "/static/dep.js", "/static/hub.js", "/static/lazy.js", "/static/page.js"}
	if got := with.Modules(); !slices.Equal(got, want) {
		t.Errorf("modules with a map %v, want %v", got, want)
	}
	if got := without.Modules(); !slices.Equal(got, want) {
		t.Errorf("modules without %v, want %v", got, want)
	}
	// The boot is the icon, the sheet, the classic script and the three
	// static modules; lazy.js and deep.js wait for the import() call.
	if n, _ := with.Boot(); n != 6 {
		t.Errorf("boot requests %d, want 6: %+v", n, with.Fetches)
	}
	for _, f := range with.Fetches {
		if f.Kind == "module" && !strings.Contains(f.CacheControl, "immutable") {
			t.Errorf("with a map, %s answered %q, want the hashed URL's immutable", f.URL, f.CacheControl)
		}
		if lazy := f.URL == h.URL("/static/lazy.js") || f.URL == h.URL("/static/deep.js"); f.Lazy != lazy {
			t.Errorf("%s lazy = %v", f.URL, f.Lazy)
		}
	}
	for _, f := range without.Fetches {
		if _, hashed := assets.PlainURL(f.URL); f.Kind == "module" && hashed {
			t.Errorf("without a map, %s is not the plain URL", f.URL)
		}
	}
}

func TestLoad_NamesEachBreach(t *testing.T) {
	h := testHandler(t)
	for _, c := range []struct {
		name, html string
		mode       Mode
		want       string
	}{
		{"a module the map lacks", strings.Replace(page(h, "/static/hub.js"), `import "/static/hub.js"`,
			`import "/static/page.js"`, 1), WithImportMap, "module /static/page.js is reached and is not in the page's import map"},
		{"a src module tag", `<script type="module" src="/static/hub.js"></script>`, NoImportMap,
			`<script type="module" src="/static/hub.js">`},
		{"a hashed src module and a plain import of it", `<script type="module" src="` + h.URL("/static/dep.js") +
			`"></script><script type="module">import "/static/hub.js";</script>`, NoImportMap, "two module instances"},
		{"a plain stylesheet URL", `<link rel="stylesheet" href="/static/a.css">`, WithImportMap,
			`name it {{assetURL "/static/a.css"}}`},
		{"a stale hash", `<script src="/static/c.0123456789abcdef.js"></script>`, WithImportMap, "stale hash"},
		{"a test file", page(h, "/static/bad.js"), WithImportMap, "/static/t.test.js is reached from the page"},
		{"a missing file", `<script type="module">import "/static/gone.js";</script>`, NoImportMap, "answered 404"},
		{"the map after a module", `<script type="module">import "/static/dep.js";</script>` +
			string(h.ImportMapTag("/static/dep.js")), WithImportMap, "the import map comes after a module script"},
		{"two maps", string(h.ImportMapTag("/static/dep.js")) + string(h.ImportMapTag("/static/dep.js")),
			WithImportMap, "a second import map"},
	} {
		r := Load(h, c.html, c.mode)
		found := false
		for _, p := range r.Problems {
			found = found || strings.Contains(p, c.want)
		}
		if !found {
			t.Errorf("%s: no problem says %q; problems: %q", c.name, c.want, r.Problems)
		}
	}
}

func TestSkeleton_EveryBranchAndEveryTemplate(t *testing.T) {
	h := testHandler(t)
	set := template.Must(template.New("").Funcs(h.FuncMap()).Parse(
		`{{define "page"}}<link rel="stylesheet" href="{{assetURL "/static/a.css"}}">` +
			`{{if .X}}<script src="{{assetURL "/static/c.js"}}"></script>{{else}}<p>{{.Y}}</p>{{end}}` +
			`{{range .Z}}<i>{{.}}</i>{{end}}{{template "part" .}}{{end}}` +
			`{{define "part"}}{{importMap "/static/hub.js"}}<script type="module">import "/static/hub.js";</script>{{end}}`))
	got, err := Skeleton(set, "page", h)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{h.URL("/static/a.css"), h.URL("/static/c.js"), "<p></p>", "<i></i>",
		string(h.ImportMapTag("/static/hub.js")), `import "/static/hub.js"`} {
		if !strings.Contains(got, want) {
			t.Errorf("skeleton lacks %q:\n%s", want, got)
		}
	}
	noProblems(t, "the skeleton's load", Load(h, got, WithImportMap))
	if _, err := Skeleton(set, "nope", h); err == nil {
		t.Error("a template that does not exist is no error")
	}
}

func TestTemplates_OnlyTheHashForm(t *testing.T) {
	h := testHandler(t)
	fsys := fstest.MapFS{
		"templates/good.html": {Data: []byte(`{{importMap "/static/hub.js"}}` +
			`<link rel="stylesheet" href="{{assetURL "/static/a.css"}}"><link rel="icon"` + "\n" +
			`  href="{{assetURL "/static/favicon.ico"}}"><script src="{{assetURL "/static/c.js"}}"></script>` +
			`<script type="module">import "/static/hub.js";</script><a href="/orders">x</a><iframe src="/processes?p=1"></iframe>` +
			`{{/* <script src="/static/c.js"> in a template comment is not a tag */}}`)},
		"templates/bad.html": {Data: []byte(`<link rel="stylesheet" href="/static/a.css">` + "\n" +
			`<script src="/static/c.js?v=1"></script>` + "\n" +
			`<script type="module" src="{{assetURL "/static/hub.js"}}"></script>` + "\n" +
			`<script src="{{assetURL "/static/gone.js"}}"></script>` + "\n" +
			`<script type="module">import "/static/hub.js?v=2";</script>` + "\n" +
			`{{importMap "/static/hub.0123456789abcdef.js"}}` + "\n")},
	}
	problems, err := Templates(fsys, h, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p, "templates/good.html") {
			t.Errorf("good.html: %s", p)
		}
	}
	for _, want := range []string{
		`templates/bad.html:1: href="/static/a.css"`,
		`templates/bad.html:2: src="/static/c.js?v=1"`,
		`templates/bad.html:3: <script type="module" src=…>`,
		`templates/bad.html:4: assetURL "/static/gone.js" is not a file the handler serves`,
		`templates/bad.html:5: inline module imports "/static/hub.js?v=2"`,
		`templates/bad.html:6: importMap entry "/static/hub.0123456789abcdef.js"`,
	} {
		found := false
		for _, p := range problems {
			found = found || strings.HasPrefix(p, want)
		}
		if !found {
			t.Errorf("no problem begins %q; problems:\n  %s", want, strings.Join(problems, "\n  "))
		}
	}
}
