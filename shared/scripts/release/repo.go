package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Modules are the five Go modules.
var Modules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

// git runs git in root and returns its stdout; the error carries git's stderr.
func git(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// gitLine is git's stdout, trimmed.
func gitLine(root string, args ...string) (string, error) {
	out, err := git(root, args...)
	return strings.TrimSpace(string(out)), err
}

// TopLevel is the work tree's root, from any directory inside it. Every git
// call here runs there: `git ls-files` and a bare path print relative to the
// directory they run in, the disk reads are relative to the root.
func TopLevel(dir string) (string, error) {
	return gitLine(dir, "rev-parse", "--show-toplevel")
}

// Version is a release version, vMAJOR.MINOR.PATCH.
type Version struct{ Major, Minor, Patch int }

var versionRe = regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// ParseVersion reads s as vMAJOR.MINOR.PATCH: no leading zeros, no suffix.
func ParseVersion(s string) (Version, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less orders versions numerically.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// Bump is the least increment a release's changes call for.
type Bump int

const (
	Patch Bump = iota
	Minor
)

func (b Bump) String() string {
	if b == Minor {
		return "MINOR"
	}
	return "PATCH"
}

// Next is the smallest version above v that b allows.
func (v Version) Next(b Bump) Version {
	if b == Minor {
		return Version{v.Major, v.Minor + 1, 0}
	}
	return Version{v.Major, v.Minor, v.Patch + 1}
}

// LastTag is the highest vMAJOR.MINOR.PATCH tag merged into HEAD, or "" when
// there is none. Highest by version, not by date, so two tags on one day or a
// tag made out of order answer the same way. A v-name that does not parse (a
// suffix, a leading zero) is not a release and is skipped.
func LastTag(root string) (string, error) {
	out, err := git(root, "tag", "--merged", "HEAD", "--list", "v*")
	if err != nil {
		return "", err
	}
	best, found := Version{}, false
	for _, name := range strings.Fields(string(out)) {
		if v, ok := ParseVersion(name); ok && (!found || best.Less(v)) {
			best, found = v, true
		}
	}
	if !found {
		return "", nil
	}
	return best.String(), nil
}

// ChangedPaths lists every path that differs between tag and the working
// tree: committed since the tag, staged, unstaged, deleted, both sides of a
// rename, and untracked. The working tree rather than HEAD, as the gate reads
// it, so a run before the commit sees what the commit will carry; CI and the
// release command run on a clean tree, where the two are the same.
func ChangedPaths(root, tag string) ([]string, error) {
	diff, err := git(root, "diff", "--name-only", "--no-renames", "-z", tag, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := git(root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, chunk := range [][]byte{diff, untracked} {
		for _, p := range strings.Split(string(chunk), "\x00") {
			if p != "" {
				set[p] = true
			}
		}
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// sourceExts are the file kinds a binary is built from: Go, SQL, and the web
// assets the www packages and shared embed (templates, JS, CSS, icons).
var sourceExts = []string{".go", ".sql", ".html", ".js", ".css", ".svg", ".ico"}

// IsSource reports whether p (slash-separated, from the repo root) is source:
// a file under one of the five modules that a binary is built from — non-test
// Go, SQL, go.mod, and the embedded templates and static assets. Tests
// (_test.go, .test.js), testdata/ and docs are not: they change no binary,
// and the changelog is written for the person deploying one.
func IsSource(p string) bool {
	mod, _, ok := strings.Cut(p, "/")
	if !ok || !slices.Contains(Modules, mod) {
		return false
	}
	if strings.Contains("/"+p, "/testdata/") {
		return false
	}
	base := path.Base(p)
	if base == "go.mod" {
		return true
	}
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.js") {
		return false
	}
	return slices.Contains(sourceExts, path.Ext(base))
}

// Registry is where one binary declares its numbered schema migrations.
type Registry struct {
	Binary string // the binary's name on a Deploy line
	File   string // slash-separated, from the repo root
}

// Registries are Core's migration chain (migrationList) and the Edge's
// (edgeMigrations). A migration declared anywhere else is not seen, so the
// working tree's copy is read for liveness: a registry that is gone or yields
// no migration is an error, never an empty set.
var Registries = []Registry{
	{"Core", "shingo-core/store/migrations.go"},
	{"Edge", "shingo-edge/store/migrations.go"},
}

// MigrationVersions returns the migration versions declared in one Go file:
// every element of a slice or array literal that is itself a literal opening
// with a version and a name, positionally — {176, "add x", fn, verify}, Core's
// shape, as it was at v0.0.14 too — or by key — {Version: 45, Name: "x", ...},
// the Edge's migrate.Migration. A version
// declared twice is an error: the runner would apply one of the two.
func MigrationVersions(file string, src []byte) ([]int, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var versions, dups []int
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if _, ok := lit.Type.(*ast.ArrayType); !ok {
			return true
		}
		for _, e := range lit.Elts {
			el, ok := e.(*ast.CompositeLit)
			if !ok {
				continue
			}
			v, ok := migrationVersion(el)
			if !ok {
				continue
			}
			if seen[v] {
				dups = append(dups, v)
				continue
			}
			seen[v] = true
			versions = append(versions, v)
		}
		return true
	})
	if len(dups) > 0 {
		return nil, fmt.Errorf("%s declares %s more than once", file, FormatVersions(dups))
	}
	sort.Ints(versions)
	return versions, nil
}

// migrationVersion reads one element literal as a migration, when it is one.
func migrationVersion(el *ast.CompositeLit) (int, bool) {
	if len(el.Elts) == 0 {
		return 0, false
	}
	if _, keyed := el.Elts[0].(*ast.KeyValueExpr); !keyed {
		if len(el.Elts) < 2 {
			return 0, false
		}
		v, ok := intLit(el.Elts[0])
		if b, isLit := el.Elts[1].(*ast.BasicLit); ok && isLit && b.Kind == token.STRING {
			return v, true
		}
		return 0, false
	}
	var v int
	var haveVersion, haveName bool
	for _, e := range el.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch k.Name {
		case "Version":
			v, haveVersion = intLit(kv.Value)
		case "Name":
			haveName = true
		}
	}
	return v, haveVersion && haveName
}

func intLit(e ast.Expr) (int, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(b.Value)
	return n, err == nil
}

// readAt reads files at rev in one git process (a process costs a quarter
// second on Windows); a path rev does not have is absent from the result.
func readAt(root, rev string, paths []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if len(paths) == 0 {
		return files, nil
	}
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(rev + ":" + p + "\n")
	}
	cmd := exec.Command("git", "-C", root, "cat-file", "--batch")
	cmd.Stdin = &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	for _, p := range paths {
		header, rest, ok := bytes.Cut(out, []byte("\n"))
		if !ok {
			return nil, fmt.Errorf("git cat-file --batch: no answer for %s:%s", rev, p)
		}
		f := strings.Fields(string(header))
		if len(f) == 3 && f[1] == "blob" {
			n, err := strconv.Atoi(f[2])
			if err != nil || n+1 > len(rest) {
				return nil, fmt.Errorf("git cat-file --batch: bad answer %q for %s:%s", header, rev, p)
			}
			files[p], out = rest[:n], rest[n+1:]
			continue
		}
		if len(f) == 3 {
			// a tree or another object type: skip its content
			n, _ := strconv.Atoi(f[2])
			if n+1 <= len(rest) {
				rest = rest[n+1:]
			}
		}
		out = rest // "<rev>:<path> missing"
	}
	return files, nil
}

// tipMigrations reads reg's versions in the working tree, where the registry
// must exist and declare one, or the reader is stale.
func tipMigrations(root string, reg Registry) ([]int, error) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(reg.File)))
	if err != nil {
		return nil, fmt.Errorf("%s migrations: %w (the registry moved: update Registries in shared/scripts/release)", reg.Binary, err)
	}
	vs, err := MigrationVersions(reg.File, src)
	if err == nil && len(vs) == 0 {
		err = fmt.Errorf("%s declares no migration (the registry moved: update Registries in shared/scripts/release)", reg.File)
	}
	return vs, err
}

// AddedMigrations is, per binary, the versions the working tree declares and
// tag does not. At the tag a registry may not exist yet (the Edge had no
// versioned migrations at v0.0.14): that is no migration.
func AddedMigrations(root, tag string) (map[string][]int, error) {
	var files []string
	for _, reg := range Registries {
		files = append(files, reg.File)
	}
	atTag, err := readAt(root, tag, files)
	if err != nil {
		return nil, err
	}
	added := map[string][]int{}
	for _, reg := range Registries {
		tip, err := tipMigrations(root, reg)
		if err != nil {
			return nil, err
		}
		var old []int
		if src, ok := atTag[reg.File]; ok {
			if old, err = MigrationVersions(reg.File, src); err != nil {
				return nil, err
			}
		}
		for _, v := range tip {
			if !slices.Contains(old, v) {
				added[reg.Binary] = append(added[reg.Binary], v)
			}
		}
	}
	return added, nil
}

// FormatVersions writes versions as ranges: v8–v175, or v3, v5–v6.
func FormatVersions(vs []int) string {
	vs = slices.Clone(vs)
	sort.Ints(vs)
	vs = slices.Compact(vs)
	var parts []string
	for i := 0; i < len(vs); {
		j := i
		for j+1 < len(vs) && vs[j+1] == vs[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, fmt.Sprintf("v%d", vs[i]))
		} else {
			parts = append(parts, fmt.Sprintf("v%d–v%d", vs[i], vs[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// ConfigPackage is a binary's config package. Its yaml keys are what an
// operator's config file may say, so adding or removing one changes what a
// deploy must carry over.
type ConfigPackage struct {
	Binary string
	Dir    string // slash-separated, from the repo root
}

// ConfigPackages are the two binaries' config packages.
var ConfigPackages = []ConfigPackage{
	{"Core", "shingo-core/config"},
	{"Edge", "shingo-edge/config"},
}

// ConfigKeys lists the yaml keys reachable from the package's Config type, as
// dotted paths: "web.port", "plcs[].name", "labels.*". A key is a field's
// yaml tag name, or for an exported field with no yaml tag its name
// lowercased (yaml.v3's default); `yaml:"-"` is no key and `,inline` adds the
// field's keys at its parent's level. Only types the package declares are
// walked into.
func ConfigKeys(files map[string][]byte) ([]string, error) {
	types := map[string]ast.Expr{}
	fset := token.NewFileSet()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := parser.ParseFile(fset, n, files[n], parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				ts := s.(*ast.TypeSpec)
				types[ts.Name.Name] = ts.Type
			}
		}
	}
	if _, ok := types["Config"]; !ok {
		return nil, errors.New("no type Config")
	}
	keys := map[string]bool{}
	onPath := map[string]bool{}
	var walk func(prefix string, t ast.Expr)
	walk = func(prefix string, t ast.Expr) {
		switch x := t.(type) {
		case *ast.Ident:
			def, ok := types[x.Name]
			if !ok || onPath[x.Name] {
				return
			}
			onPath[x.Name] = true
			walk(prefix, def)
			delete(onPath, x.Name)
		case *ast.ParenExpr:
			walk(prefix, x.X)
		case *ast.StarExpr:
			walk(prefix, x.X)
		case *ast.ArrayType:
			walk(prefix+"[]", x.Elt)
		case *ast.MapType:
			walk(joinKey(prefix, "*"), x.Value)
		case *ast.StructType:
			for _, f := range x.Fields.List {
				name, inline, skip := yamlKey(f)
				switch {
				case skip:
				case inline:
					walk(prefix, f.Type)
				default:
					k := joinKey(prefix, name)
					keys[k] = true
					walk(k, f.Type)
				}
			}
		}
	}
	// From the type name, so a field typed Config is a cycle like any other.
	walk("", &ast.Ident{Name: "Config"})
	out := make([]string, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func joinKey(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// yamlKey is the key a struct field decodes from: its tag's name, or its own
// name lowercased; inline when the tag says so; skip for `yaml:"-"`, an
// unexported field, or an embedded one that is not inline.
func yamlKey(f *ast.Field) (name string, inline, skip bool) {
	var tag string
	var tagged bool
	if f.Tag != nil {
		if s, err := strconv.Unquote(f.Tag.Value); err == nil {
			tag, tagged = reflect.StructTag(s).Lookup("yaml")
		}
	}
	opts := strings.Split(tag, ",")
	if tagged && opts[0] == "-" {
		return "", false, true
	}
	if tagged && slices.Contains(opts[1:], "inline") {
		return "", true, false
	}
	if len(f.Names) == 0 {
		return "", false, true
	}
	if !f.Names[0].IsExported() {
		return "", false, true
	}
	if tagged && opts[0] != "" {
		return opts[0], false, false
	}
	return strings.ToLower(f.Names[0].Name), false, false
}

// isConfigSource is a config package file ConfigKeys reads: non-test Go.
func isConfigSource(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

// tipConfigFiles reads the package's non-test Go files in the working tree.
func tipConfigFiles(root, dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, fmt.Errorf("config package %s: %w", dir, err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !isConfigSource(e.Name()) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = src
	}
	return files, nil
}

// tagConfigFiles reads every config package's non-test Go files at rev, in
// two git processes, keyed by package directory. A package rev does not have
// is absent.
func tagConfigFiles(root, rev string) (map[string]map[string][]byte, error) {
	args := []string{"ls-tree", "--full-tree", "--name-only", rev, "--"}
	for _, pkg := range ConfigPackages {
		args = append(args, pkg.Dir+"/")
	}
	out, err := git(root, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p != "" && isConfigSource(p) {
			paths = append(paths, p)
		}
	}
	files, err := readAt(root, rev, paths)
	if err != nil {
		return nil, err
	}
	byDir := map[string]map[string][]byte{}
	for p, src := range files {
		d := path.Dir(p)
		if byDir[d] == nil {
			byDir[d] = map[string][]byte{}
		}
		byDir[d][path.Base(p)] = src
	}
	return byDir, nil
}

// KeyDelta is the yaml keys one config package gained and lost.
type KeyDelta struct{ Added, Removed []string }

// ConfigDelta is, per binary, the config keys the working tree has and tag
// does not, and the reverse.
func ConfigDelta(root, tag string) (map[string]KeyDelta, error) {
	atTag, err := tagConfigFiles(root, tag)
	if err != nil {
		return nil, err
	}
	delta := map[string]KeyDelta{}
	for _, pkg := range ConfigPackages {
		tipFiles, err := tipConfigFiles(root, pkg.Dir)
		if err != nil {
			return nil, err
		}
		tip, err := ConfigKeys(tipFiles)
		if err != nil {
			return nil, fmt.Errorf("%s config at the working tree: %w", pkg.Binary, err)
		}
		var old []string
		if files := atTag[pkg.Dir]; files != nil {
			if old, err = ConfigKeys(files); err != nil {
				return nil, fmt.Errorf("%s config at %s: %w", pkg.Binary, tag, err)
			}
		}
		var d KeyDelta
		for _, k := range tip {
			if !slices.Contains(old, k) {
				d.Added = append(d.Added, k)
			}
		}
		for _, k := range old {
			if !slices.Contains(tip, k) {
				d.Removed = append(d.Removed, k)
			}
		}
		if len(d.Added)+len(d.Removed) > 0 {
			delta[pkg.Binary] = d
		}
	}
	return delta, nil
}
