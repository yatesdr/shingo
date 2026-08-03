package www

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"shingocore/fleet"
)

// rdsProxyTimeout is the HTTP timeout for proxied RDS explorer requests.
const rdsProxyTimeout = 15 * time.Second

func (h *Handlers) handleFleetExplorer(w http.ResponseWriter, r *http.Request) {
	baseURL := ""
	if vp, ok := h.engine.Fleet().(fleet.VendorProxy); ok {
		baseURL = vp.BaseURL()
	}
	data := map[string]any{
		"Page":         "fleet-explorer",
		"FleetBaseURL": baseURL,
	}
	h.render(w, r, "rds_explorer.html", data)
}

// apiFleetProxy forwards an arbitrary request to the fleet vendor API and returns the raw response.
func (h *Handlers) apiFleetProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	vp, ok := h.engine.Fleet().(fleet.VendorProxy)
	if !ok {
		h.jsonError(w, "fleet backend does not support API proxy", http.StatusNotImplemented)
		return
	}

	var req struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}
	if !h.parseJSON(w, r, &req) {
		return
	}

	if req.Method == "" {
		req.Method = "GET"
	}
	req.Method = strings.ToUpper(req.Method)
	if !strings.HasPrefix(req.Path, "/") {
		req.Path = "/" + req.Path
	}

	baseURL := vp.BaseURL()
	fullURL := baseURL + req.Path

	var bodyReader io.Reader
	if req.Body != "" {
		bodyReader = strings.NewReader(req.Body)
	}

	httpReq, err := http.NewRequest(req.Method, fullURL, bodyReader)
	if err != nil {
		h.jsonError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	client := &http.Client{Timeout: rdsProxyTimeout}
	resp, err := client.Do(httpReq)
	elapsed := time.Since(start)

	if err != nil {
		// The proxy could not do its job, so this is not a success.
		//
		// The distinction this handler has to keep is between "the vendor
		// answered and I am showing you what it said" and "I never got an
		// answer". The first is the whole point of an explorer — a 404 or a 400
		// from the vendor is a legitimate thing to go looking for, and it is
		// relayed below at 200 with the vendor's own code in the body. This is
		// the second: client.Do fails only for DNS, refused connections, TLS,
		// or the timeout, never for an HTTP error status. There is no vendor
		// response to show, which is what the status_code of 0 was already
		// admitting.
		//
		// 502 rather than 500: Core is the gateway here and the thing that
		// failed is upstream of it. The envelope is unchanged so the page
		// renders exactly as before — it reads these fields and never checks
		// the status.
		h.fleetProxyFailed(w, http.StatusBadGateway, map[string]any{
			"error":       err.Error(),
			"url":         fullURL,
			"method":      req.Method,
			"elapsed_ms":  elapsed.Milliseconds(),
			"status_code": 0,
		})
		return
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		// Reached the vendor, could not read what it said — the connection
		// dropped mid-body, or the timeout fired during the read. This error
		// used to be discarded, which sent a TRUNCATED body out under the
		// vendor's own status code: a proxy failure wearing a vendor success,
		// and the one an operator would most reasonably misread as a vendor
		// bug.
		h.fleetProxyFailed(w, http.StatusBadGateway, map[string]any{
			"error":       "could not read the vendor's response: " + readErr.Error(),
			"url":         fullURL,
			"method":      req.Method,
			"elapsed_ms":  elapsed.Milliseconds(),
			"status_code": resp.StatusCode,
		})
		return
	}

	// Try to parse as JSON for the response
	var jsonBody any
	isJSON := false
	if err := json.Unmarshal(respBody, &jsonBody); err == nil {
		isJSON = true
	}

	result := map[string]any{
		"url":         fullURL,
		"method":      req.Method,
		"status_code": resp.StatusCode,
		"elapsed_ms":  elapsed.Milliseconds(),
		"headers":     flattenHeaders(resp.Header),
	}
	if isJSON {
		result["body"] = jsonBody
	} else {
		result["body_text"] = string(respBody)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// fleetProxyFailed answers a request the proxy could not carry out.
//
// It exists instead of jsonError because the explorer page renders from the
// whole envelope — url, method, elapsed_ms — and jsonError sends only the
// error string, which would leave the page showing "undefinedms" next to a
// blank request line. The status is what changed; the body is what it always
// was.
func (h *Handlers) fleetProxyFailed(w http.ResponseWriter, code int, envelope map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(envelope)
}

func flattenHeaders(h http.Header) map[string]string {
	flat := make(map[string]string, len(h))
	for k, v := range h {
		flat[k] = strings.Join(v, ", ")
	}
	return flat
}
