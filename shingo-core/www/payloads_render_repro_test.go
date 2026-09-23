package www

import (
	"bytes"
	"html/template"
	"testing"

	"shingocore/store"
	"shingocore/store/payloads"
)

// Reproduction: the payloads page's containment cell fails mid-render on the
// live data (the page truncates after two rows, with "template error" written
// into the data-unrouted attribute). The render needs no database: parse the
// template the way the router does and execute it against the same data
// shape, so the underlying html/template error surfaces verbatim.
func TestPayloadsTemplateRenderRepro(t *testing.T) {
	base := template.New("").Funcs(templateFuncs(nil))
	base = template.Must(base.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
	clone := template.Must(base.Clone())
	clone = template.Must(clone.ParseFS(templateFS, "templates/payloads.html"))

	data := map[string]any{
		"Page": "payloads",
		"Payloads": []*payloads.Payload{
			{ID: 1, Code: "ASSY", Description: "repro"},
			{ID: 3, Code: "BRKT", Description: "repro"},
		},
		"BinTypes":        []any{},
		"CompatNodes":     map[int64][]string{1: {"ALN_002"}, 3: {"ALN_004"}},
		"PayloadBinTypes": map[int64][]string{},
		"ContainmentState": map[string]*store.PayloadContainmentRow{
			"ASSY": {PayloadCode: "ASSY", Active: true, Reason: "test reason", ActivatedBy: "admin"},
		},
		"UnroutedProducers": map[string]string{
			"BRKT": "LOADER-CLIP, Test - FG",
		},
		"Authenticated": true,
	}

	var out bytes.Buffer
	err := clone.ExecuteTemplate(&out, "layout", data)
	if err != nil {
		t.Fatalf("template error: %v", err)
	}
	t.Logf("rendered %d bytes OK", out.Len())
}
