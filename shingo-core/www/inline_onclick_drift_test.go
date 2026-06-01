package www

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoInlineEventHandlersInTemplates pins the "no inline event
// handlers" convention from the style guide (§ Event handling).
// Mirrors the equivalent test in shingo-edge/www; see that file's
// comment for the rationale.
func TestNoInlineEventHandlersInTemplates(t *testing.T) {
	eventRe := regexp.MustCompile(`(?i)\bon(click|change|input|blur|keydown|submit|focus|keyup|mousedown|mouseup)\s*=`)
	allowlist := []string{
		// (empty — single landing pad for justified exceptions)
	}

	root := "templates"
	err := fs.WalkDir(templateFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(p) != ".html" {
			return nil
		}
		for _, allowed := range allowlist {
			if strings.HasSuffix(p, allowed) {
				return nil
			}
		}
		body, err := fs.ReadFile(templateFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if eventRe.MatchString(line) {
				t.Errorf("%s:%d inline event handler — use data-action[-event] + delegateActions instead\n  %s",
					p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}

// TestNoMalformedDataActionInJS guards the bf5ed4b regression class: the
// mechanical onclick="fn(arg)" -> data-action="fn:arg" migration left a
// stray ")" (and in two cases a literal backslash-quote \') inside
// data-action attribute values built by string concatenation in the page
// JS. delegateActions splits the value on ":", so a trailing ")" rides
// into the argument ("5)" instead of "5") and a literal \' is never
// evaluated — both silently break the button. The template drift test
// above only scans templates/*.html, which is why this shipped.
//
// Signatures (scoped to lines that build a data-action attribute):
//   - ')"   a JS string literal that opens with ")" right where the
//     attribute should close — the stray-paren shape.
//   - \'    a backslash-escaped apostrophe used where a real string
//     delimiter belongs — the literal-quote shape.
//
// Note escapeHtml(x)'s parens are code, not string content, and are
// always followed by " + " rather than the attribute-closing quote, so
// they do not match.
func TestNoMalformedDataActionInJS(t *testing.T) {
	strayParen := regexp.MustCompile(`'\)"`)
	literalQuote := regexp.MustCompile(`\\'`)

	root := "static/pages"
	err := fs.WalkDir(staticFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".js" {
			return nil
		}
		body, err := fs.ReadFile(staticFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "data-action") {
				continue
			}
			if strayParen.MatchString(line) || literalQuote.MatchString(line) {
				t.Errorf("%s:%d malformed data-action value (stray ')' or backslash-quote) — emit a clean \"fn:' + arg + '\"\n  %s",
					p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk static/pages: %v", err)
	}
}
