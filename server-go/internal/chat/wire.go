// Package chat is the completion proxy: it translates the webui request into
// what the engine reads, calls it, and rewrites the streamed answer so the UI
// gets the field names and the timings a llama.cpp server would have sent.
package chat

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// WriteJSON answers with a JSON body.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		WriteMessage(w, http.StatusInternalServerError, "serialization failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	// net/http drops the body of a HEAD response, so writing it unconditionally
	// is what Node's res.end does here.
	_, _ = w.Write(body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteMessage answers with {"error":{"message":...}}.
func WriteMessage(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": map[string]any{"message": msg}})
}

// WriteError answers with {"error":...}, the shorter shape the stubs use.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": msg})
}

// WriteUnavailable answers like a llama.cpp route this backend does not have.
func WriteUnavailable(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"success": false, "error": msg})
}

// OmitMissing drops keys the adapter could not fill in. The Node version leaves
// them out of the body, because an undefined value is not serialized.
func OmitMissing(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// WriteDisabled answers the way llama-server does for a feature that was not
// enabled at startup, which the webui turns into its own hint text.
func WriteDisabled(w http.ResponseWriter) {
	WriteJSON(w, http.StatusForbidden, map[string]any{
		"error": map[string]any{"message": "this feature is disabled", "type": "feature_disabled"},
	})
}
