// Package merkleservice implements the mock Merkle Service: an HTTP server that
// accepts /watch registrations, manages subtrees, and delivers BUMP proofs to
// callback URLs when the simulated block is complete.
package merkleservice

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
)

// Server is the Merkle Service HTTP server.
type Server struct {
	cfg     *config.Config
	mc      *metrics.Collector
	reg     *Registry
	ln      net.Listener
	blockCh chan struct{} // closed once BUMP delivery is complete
	ctx     context.Context
}

// NewServer creates a Server bound to ln, wiring up the Registry.
// ln may be nil for direct-mode operation (no HTTP serving).
func NewServer(cfg *config.Config, mc *metrics.Collector, ln net.Listener) *Server {
	s := &Server{
		cfg:     cfg,
		mc:      mc,
		reg:     newRegistry(cfg, mc),
		ln:      ln,
		blockCh: make(chan struct{}),
		ctx:     context.Background(),
	}
	return s
}

// Start runs the HTTP server until ctx is cancelled.
// Panics if ln is nil (use direct mode instead).
func (s *Server) Start(ctx context.Context) {
	s.ctx = ctx
	h := &handler{reg: s.reg, srv: s}
	srv := &http.Server{Handler: h}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("merkle service listening", "addr", s.ln.Addr())
	if err := srv.Serve(s.ln); err != nil && err != http.ErrServerClosed {
		slog.Error("merkle service error", "err", err)
	}
}

// WaitForBlock blocks until BUMP delivery is complete (or ctx is cancelled).
func (s *Server) WaitForBlock(ctx context.Context) {
	select {
	case <-s.blockCh:
	case <-ctx.Done():
	}
}

// AddTxIDDirect is the in-process fast path for direct-mode operation.
// It bypasses HTTP/JSON encoding and calls the registry directly.
// Returns a *BlockFinalizedEvent (as interface{}) when the block is complete,
// or nil otherwise. The caller should pass the event to ProcessBlock.
func (s *Server) AddTxIDDirect(txid chainhash.Hash, token, callbackURL string) interface{} {
	cb := CallbackInfo{URL: callbackURL, Token: token}
	return s.reg.AddTxIDDirect(txid, token, cb)
}

// ProcessBlock runs the BUMP assembly and delivery pipeline for a completed
// block. Used in direct mode where the caller drives the block lifecycle
// instead of the HTTP handler.
func (s *Server) ProcessBlock(evt interface{}) {
	blockEvt, ok := evt.(*BlockFinalizedEvent)
	if !ok || blockEvt == nil {
		return
	}
	s.onBlockComplete(blockEvt)
}

// deliveryTimeout is the maximum time allowed for BUMP assembly + delivery
// after the block is complete.  It is independent of the main run context so
// that a near-deadline block finalisation still delivers all BUMPs.
const deliveryTimeout = 2 * time.Minute

// onBlockComplete is called (in a goroutine) by the handler when the block is
// full.  It drives the BUMP assembly and delivery pipeline.
func (s *Server) onBlockComplete(evt *BlockFinalizedEvent) {
	slog.Info("block finalizing",
		"subtrees", len(evt.SubtreeRoots),
		"tokens", len(evt.Callbacks),
	)

	// Use a fresh context so that a main-context cancellation or timeout
	// that coincides with block finalisation does not abort BUMP delivery.
	deliveryCtx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	defer cancel()

	processBUMPs(
		deliveryCtx,
		s.cfg.BlockHeight,
		s.cfg.SubtreeHeight(),
		s.cfg.TopTreeHeight(),
		s.cfg.BlockMerkleHeight(),
		s.mc,
		evt,
		s.cfg.CallbackURL(),
		s.cfg.DumpBUMPFile,
	)

	slog.Info("block pipeline complete")
	// Close blockCh only once.
	select {
	case <-s.blockCh:
	default:
		close(s.blockCh)
	}
}
