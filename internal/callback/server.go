// Package callback implements a mock HTTP server that receives BUMP deliveries
// from the Merkle Service and logs timing + payload metrics.
package callback

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/bsv-blockchain/stumpt/internal/metrics"
)

// Server is the mock callback receiver.
type Server struct {
	ln        net.Listener
	mc        *metrics.Collector
	blockTime time.Time // set by SetBlockTime when the block is announced
}

// NewServer creates a new callback Server bound to ln.
func NewServer(ln net.Listener, mc *metrics.Collector) *Server {
	return &Server{ln: ln, mc: mc}
}

// SetBlockTime records the instant the block was announced so we can measure
// end-to-end delivery latency.
func (s *Server) SetBlockTime(t time.Time) { s.blockTime = t }

// Start runs the HTTP server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleBUMP)

	srv := &http.Server{Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("callback server listening", "addr", s.ln.Addr())
	if err := srv.Serve(s.ln); err != nil && err != http.ErrServerClosed {
		slog.Error("callback server error", "err", err)
	}
}

// handleBUMP receives a raw BUMP binary POST from the merkle service.
//
// Expected headers:
//
//	X-Callback-Token : the submitter token
//	X-Block-Height   : uint32 block height (informational)
//
// Body: raw BUMP bytes (BRC-74 binary format).
func (s *Server) handleBUMP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	arrival := time.Now()

	token := r.Header.Get("X-Callback-Token")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("callback: read body", "err", err)
		http.Error(w, "read error", http.StatusInternalServerError)
		return
	}

	size := len(body)
	var latency time.Duration
	if !s.blockTime.IsZero() {
		latency = arrival.Sub(s.blockTime)
	}

	s.mc.RecordCallback(latency, size)

	slog.Info("callback received",
		"token", token,
		"bump_bytes", size,
		"latency", latency,
		"bump_hex_prefix", bumpPrefix(body),
	)

	w.WriteHeader(http.StatusOK)
}

// bumpPrefix returns the first 16 bytes of the BUMP as hex for logging.
func bumpPrefix(b []byte) string {
	if len(b) > 16 {
		b = b[:16]
	}
	return hex.EncodeToString(b)
}
