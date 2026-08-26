package www

import (
	"compress/gzip"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// renderCompressionHandlers builds the minimum a render path touches: a
// template set and a session store. No engine or DB is involved — render only
// looks up the template and asks whether the request is authenticated.
func renderCompressionHandlers(t *testing.T) *Handlers {
	t.Helper()
	// Long enough that gzip has something to work with; the real pages are
	// 10 KB to 120 KB.
	body := strings.Repeat("<tr><td>row</td></tr>", 2000)
	return &Handlers{
		tmpls: map[string]*template.Template{
			"page": template.Must(template.New("layout").Parse(body)),
			"bare": template.Must(template.New("bare").Parse(body)),
		},
		sessions: newSessionStore("render-compression-test"),
	}
}

// serveCompressed runs fn through a router shaped like NewRouter's compression
// group and returns the response.
func serveCompressed(fn http.HandlerFunc) *http.Response {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress(5))
		r.Get("/", fn)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Result()
}

// TestRenderPathsAreCompressed is the regression guard for the missing
// Content-Type. chi's Compress reads that header at WriteHeader time and skips
// compression when it is empty; net/http then sniffs the body and stamps
// text/html on the way out, so a response that never compressed still LOOKS
// correct and is ~10-20x too large. Nothing errors, so only a test catches it.
//
// A new render helper that forgets shared.SetHTMLContentType fails here.
func TestRenderPathsAreCompressed(t *testing.T) {
	h := renderCompressionHandlers(t)

	cases := []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"render", func(w http.ResponseWriter, r *http.Request) {
			h.render(w, r, "page", map[string]any{})
		}},
		{"renderBare", func(w http.ResponseWriter, r *http.Request) {
			h.renderBare(w, "bare", map[string]any{})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := serveCompressed(tc.fn)
			defer resp.Body.Close()

			if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip — the handler wrote "+
					"before setting Content-Type, so the compression middleware "+
					"skipped this response", got)
			}

			zr, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatalf("response is not valid gzip: %v", err)
			}
			defer zr.Close()
			plain, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("read gzip body: %v", err)
			}
			if len(plain) == 0 {
				t.Fatal("decompressed body is empty — the template did not render")
			}
		})
	}
}
