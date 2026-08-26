package shared

import "net/http"

// HTMLContentType is the value both services stamp on a rendered page.
const HTMLContentType = "text/html; charset=utf-8"

// SetHTMLContentType declares a response as HTML. It MUST be called before the
// first write.
//
// Without it the response still reaches the browser correctly labelled and
// roughly ten to twenty times larger than it needs to be, which is why the
// omission is invisible in normal use and has to be guarded by a test.
//
// The mechanism: chi's middleware.Compress decides whether to compress inside
// WriteHeader, by reading Content-Type and matching it against its allowed
// list. A handler that writes without setting the header first gives it an
// empty string, mime.ParseMediaType fails, and compression is skipped for that
// response. net/http then sniffs the body and stamps text/html on the way out
// — after the decision was made. Nothing errors and the header is correct, so
// the only symptom is size.
//
// Measured live at Springfield 2026-08-21, both services affected:
//
//	edge /orders    347,495 B uncompressed -> 16,874 B gzipped (20.5x)
//	core /nodes     124,564 B uncompressed -> 10,672 B gzipped (11.7x)
//
// Every full page the shop floor loads over WiFi was paying that.
//
// Do NOT call this on the SSE endpoints. They are deliberately registered
// outside the compression group: a compressing writer buffers, which defeats
// the streaming flush and fills the per-client send queue.
func SetHTMLContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", HTMLContentType)
}
