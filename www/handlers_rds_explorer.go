package www

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (h *Handlers) handleRDSExplorer(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page":          "rds",
		"Authenticated": h.isAuthenticated(r),
		"RDSBaseURL":    h.engine.AppConfig().RDS.BaseURL,
	}
	h.render(w, "rds_explorer.html", data)
}

// apiRDSProxy forwards an arbitrary request to the RDS API and returns the raw response.
func (h *Handlers) apiRDSProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Body   string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Method == "" {
		req.Method = "GET"
	}
	req.Method = strings.ToUpper(req.Method)
	if !strings.HasPrefix(req.Path, "/") {
		req.Path = "/" + req.Path
	}

	baseURL := h.engine.AppConfig().RDS.BaseURL
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
	client := &http.Client{Timeout: 15 * time.Second}
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
