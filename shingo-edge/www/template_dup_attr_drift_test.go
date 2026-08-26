package www

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A DUPLICATE ATTRIBUTE IS INVISIBLE AND LOSSY.
//
// `<div class="card" class="mb-4">` is valid enough to render: every HTML
// parser keeps the FIRST occurrence and silently drops the rest. So the second
// value never applies, Go's html/template does not warn, no linter in this
// repo looks for it, and the page comes out nearly right — just without the
// spacing or layout somebody deliberately wrote.
//
// 23 tags carried one, 22 of them in processes.html, which is why the claim
// editor's form-group--nogap, mb-4 and mt-3 had never once taken effect. The
// repair was mechanical — merge the values into a single attribute — and this
// is what stops the next one, because nothing else in the toolchain can see it.
//
// Scoped to duplicates on ONE tag, deliberately. It does not police which
// attributes exist or what they hold; that is the template's business, and a
// broader rule here would be a style opinion wearing a correctness test's
// clothes.
func TestTemplatesHaveNoDuplicateAttributes(t *testing.T) {
	// '<name', then anything that is not an angle bracket outside quotes, then
	// '>'. Quoted runs are consumed whole so an attribute value containing '>'
	// cannot end the tag early.
	tagRe := regexp.MustCompile(`(?s)<([a-zA-Z][-a-zA-Z0-9]*)((?:[^<>"']|"[^"]*"|'[^']*')*?)>`)
	attrRe := regexp.MustCompile(`(?s)(?:^|\s)([-a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=`)

	var offenders []string
	err := fs.WalkDir(templatesFS, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".html" {
			return err
		}
		body, err := fs.ReadFile(templatesFS, p)
		if err != nil {
			return err
		}
		src := string(body)
		for _, tag := range tagRe.FindAllStringSubmatchIndex(src, -1) {
			name := src[tag[2]:tag[3]]
			attrs := src[tag[4]:tag[5]]
			seen := map[string]bool{}
			for _, m := range attrRe.FindAllStringSubmatch(attrs, -1) {
				a := strings.ToLower(m[1])
				if seen[a] {
					line := strings.Count(src[:tag[0]], "\n") + 1
					offenders = append(offenders, fmt.Sprintf("%s:%d <%s> repeats %q", p, line, name, a))
				}
				seen[a] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d tag(s) carry a duplicate attribute. The parser keeps the FIRST and drops the rest, "+
			"so the later value never applies and nothing else reports it. Merge them into one "+
			"attribute — class=\"a b\", style=\"x; y\":\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
