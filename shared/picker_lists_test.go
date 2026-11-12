package shared

import (
	"regexp"
	"strings"
	"testing"
)

// No page builds its own node or part list (brief v2 S3's gate). A page reads
// its nodes and parts once and draws every field on them through the shared
// adapters, shared/node-picker.js and shared/part-picker.js, so a rule written
// for one field cannot drift into another page's copy: the Orders page's JS
// builder and the Test orders page's Go one sorted two ways, and only one of
// them looked past one level.
//
// Two rules, over the sources the browser reads (sharedCensusSources):
//
//   - a page module that reads a node or part list (Core's /api/nodes,
//     /api/payloads, /api/payloads/templates, the Edge's /api/core-nodes and
//     /api/payload-catalog, or a list the page was rendered with) imports an
//     adapter;
//   - no template fills a <select> from a {{range}} over nodes or parts
//     (.Nodes, .CoreNodes, .Payloads, ...): a list the server holds goes to
//     the page as data and is drawn by an adapter.
//
// The exceptions say why. Each is a ratchet: an entry that no longer matches
// fails, so the change that moves a page deletes its entry.

// pageListReaders are the page modules that read a node or part list and
// import no adapter, by repo path. None: the Processes page, the last, came
// onto the adapters in S3 (s3-processes).
var pageListReaders = map[string]string{}

// templateRangeSelects are the <select>s a template fills from a {{range}},
// as "path id".
var templateRangeSelects = map[string]string{
	"shingo-edge/www/templates/manual-order.html mo-node": "this Edge's own process nodes, valued by process-node id: not Core's node list",
	"shingo-edge/www/templates/production.html mo-node":   "this Edge's own process nodes, valued by process-node id: not Core's node list",
}

var (
	pageListReadRe = regexp.MustCompile(`['"]/api/(?:nodes|core-nodes|payloads|payloads/templates|payload-catalog)['"?]|\bPAGE_NODES\b|\bPAGE_PAYLOADS\b|dataset\.coreNodes\b|dataset\.nodes\b|dataset\.payloads\b`)
	pageAdapterRe  = regexp.MustCompile(`from '/static/shared/(?:node|part)-picker\.js'`)
	tmplSelectRe   = regexp.MustCompile(`(?s)<select([^>]*)>(.*?)</select>`)
	tmplIDRe       = regexp.MustCompile(`\bid="([^"]+)"`)
	tmplListRange  = regexp.MustCompile(`\{\{range \$?\.\w*(?:Nodes|Payloads|Catalog)\b`)
)

func TestPagesBuildNoNodeOrPartList(t *testing.T) {
	t.Parallel()
	seenReader := map[string]bool{}
	seenSelect := map[string]bool{}
	readers := 0
	for _, s := range sharedCensusSources(t) {
		if strings.HasPrefix(s.path, "shared/") || strings.HasSuffix(s.path, ".test.js") {
			continue
		}
		if strings.HasSuffix(s.path, ".js") && pageListReadRe.MatchString(s.code) {
			readers++
			if !pageAdapterRe.MatchString(s.code) {
				if _, ok := pageListReaders[s.path]; ok {
					seenReader[s.path] = true
				} else {
					t.Errorf("%s reads a node or part list (%s) and draws it itself: draw it through /static/shared/node-picker.js or part-picker.js",
						s.path, pageListReadRe.FindString(s.code))
				}
			}
		}
		if strings.HasSuffix(s.path, ".html") {
			for _, m := range tmplSelectRe.FindAllStringSubmatch(s.code, -1) {
				if !tmplListRange.MatchString(m[2]) {
					continue
				}
				id := ""
				if im := tmplIDRe.FindStringSubmatch(m[1]); im != nil {
					id = im[1]
				}
				key := s.path + " " + id
				if _, ok := templateRangeSelects[key]; ok {
					seenSelect[key] = true
					continue
				}
				t.Errorf("%s fills <select id=%q> from a {{range}} over nodes or parts: send the list as data and draw it through a shared adapter", s.path, id)
			}
		}
	}
	for path, why := range pageListReaders {
		if !seenReader[path] {
			t.Errorf("pageListReaders lists %s (%s), which imports an adapter now or reads no list: delete the entry", path, why)
		}
	}
	for key, why := range templateRangeSelects {
		if !seenSelect[key] {
			t.Errorf("templateRangeSelects lists %q (%s), which no template fills from a {{range}} now: delete the entry", key, why)
		}
	}
	if readers < 5 {
		t.Fatalf("found only %d page modules reading a node or part list; the census walk is broken, not the pages", readers)
	}
}
