package www

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"shingoedge/domain"
)

// TestChangeoverHistory_RowSaysItsRunOut: a history row whose changeover has a
// measured run-out says "ran out N after the cutover"; one without says "--".
func TestChangeoverHistory_RowSaysItsRunOut(t *testing.T) {
	tmpl := template.Must(template.New("").
		Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))
	at := time.Date(2026, 10, 26, 14, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "changeover.html", map[string]any{
		"Page": "changeover", "ActiveProcessID": int64(1),
		"ChangeoverHistory": []domain.Changeover{
			{ID: 7, StartedAt: at, CompletedAt: &at, State: domain.ChangeoverCompleted},
			{ID: 8, StartedAt: at, CompletedAt: &at, State: domain.ChangeoverCompleted},
		},
		"RunOuts": map[int64]int64{7: 16},
	}); err != nil {
		t.Fatalf("render changeover.html: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "ran out 16 after the cutover") != 1 {
		t.Errorf("the history does not say changeover 7 ran out 16 after the cutover, once:\n%s", out)
	}
}
