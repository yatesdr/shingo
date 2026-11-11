package www

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestKeyRouteRowIsDeadCSS pins census bug 14 as it is: shared/components.css
// styles `.key-route-row` ("a node picker that takes the width, plus its
// remove button") and nothing either page set draws one. The shared picker
// work either gives the rule a user or deletes it; both show here as the
// half of this pin that moved.
func TestKeyRouteRowIsDeadCSS(t *testing.T) {
	t.Parallel()
	repo := filepath.Join("..", "..")
	css, err := os.ReadFile(filepath.Join(repo, "shared", "components.css"))
	if err != nil {
		t.Fatalf("read shared/components.css: %v", err)
	}
	rules := regexp.MustCompile(`(?m)^\.key-route-row[^{]*\{`).FindAllString(string(css), -1)
	if len(rules) != 3 {
		t.Errorf("shared/components.css: %d .key-route-row rules, want the 3 the census found (row, row + row, row > select): %q", len(rules), rules)
	}

	var users []string
	for _, dir := range []string{
		filepath.Join(repo, "shingo-edge", "www", "static"), filepath.Join(repo, "shingo-edge", "www", "templates"),
		filepath.Join(repo, "shingo-core", "www", "static"), filepath.Join(repo, "shingo-core", "www", "templates"),
		filepath.Join(repo, "shared"),
	} {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(p) {
			case ".js", ".html", ".css", ".go":
			default:
				return nil
			}
			if strings.HasSuffix(p, "components.css") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "key-route-row") {
				users = append(users, filepath.ToSlash(p))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(users) > 0 {
		t.Errorf(".key-route-row has a user now (census bug 14 moved; name the change): %v", users)
	}
}
