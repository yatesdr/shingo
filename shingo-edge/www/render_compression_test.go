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

// TestRenderTemplateIsCompressed is the regression guard for the missing
// Content-Type. chi's Compress reads that header at WriteHeader time and skips
// compression when it is empty; net/http then sniffs the body and stamps
// text/html on the way out, so a response that never compressed still LOOKS
// correct and is ~20x too large. Nothing errors, so only a test catches it.
//
// This is the path every full operator page goes through — material, orders,
// changeover, production, operator stations, diagnostics, admin. Measured live
// at Springfield, /orders was 347,495 B where 16,874 B would do, on WiFi.
//
// A new render helper that forgets shared.SetHTMLContentType fails here.
func TestRenderTemplateIsCompressed(t *testing.T) {
	// Long enough that gzip has something to work with; the real pages are
	// tens to hundreds of KB.
	body := strings.Repeat("<tr><td>row</td></tr>", 2000)
	h := &Handlers{
		tmpl:     template.Must(template.New("page").Parse(body)),
		sessions: newSessionStore("render-compression-test"),
	}

	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress(5))
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			h.renderTemplate(w, req, "page", map[string]any{})
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip — renderTemplate wrote before "+
			"setting Content-Type, so the compression middleware skipped this response", got)
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
	if len(plain) != len(body) {
		t.Fatalf("decompressed %d bytes, want %d — the template did not render fully",
			len(plain), len(body))
	}
}
