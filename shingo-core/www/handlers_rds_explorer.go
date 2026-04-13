package www

import (
	"encoding/json"
	"fmt"
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
		"Page":          "fleet-explorer",
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"error":       err.Error(),
			"url":         fullURL,
			"method":      req.Method,
			"elapsed_ms":  elapsed.Milliseconds(),
			"status_code": 0,
		})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

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

func flattenHeaders(h http.Header) map[string]string {
	flat := make(map[string]string, len(h))
	for k, v := range h {
		flat[k] = fmt.Sprintf("%s", strings.Join(v, ", "))
	}
	return flat
}
