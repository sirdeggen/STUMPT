package merkleservice

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// handler wraps the Registry and routes HTTP requests.
type handler struct {
	reg *Registry
	srv *Server
}

// ServeHTTP dispatches to the correct sub-handler.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/watch":
		h.handleWatch(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleWatch processes POST /watch.
//
// Request body:
//
//	{
//	  "txid": "<64-char raw-bytes hex>",
//	  "callback": { "url": "http://...", "token": "secret-N" }
//	}
//
// Response: 202 Accepted (immediately, before proof pre-computation).
func (h *handler) handleWatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Warn("watch: read body", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var req WatchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Warn("watch: unmarshal", "err", err)
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}

	if req.TxID == "" {
		http.Error(w, "missing txid", http.StatusBadRequest)
		return
	}

	// Respond immediately — sealing happens synchronously inside AddTxID but
	// only on subtree boundaries (~once per 10 seconds at default rate).
	w.WriteHeader(http.StatusAccepted)

	evt := h.reg.AddTxID(req.TxID, req.Callback.Token, req.Callback)
	if evt != nil {
		// Non-nil means the block is complete; hand off to the server.
		go h.srv.onBlockComplete(evt)
	}
}
