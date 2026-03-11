// Package merkleservice implements the mock Merkle Service: an HTTP server that
// accepts /watch registrations, manages subtrees, and delivers BUMP proofs to
// callback URLs when the simulated block is complete.
package merkleservice

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
)

// Server is the Merkle Service HTTP server.
type Server struct {
	cfg     *config.Config
	mc      *metrics.Collector
	reg     *Registry
	blockCh chan struct{} // closed once BUMP delivery is complete
}

// NewServer creates a Server, wiring up the Registry.
func NewServer(cfg *config.Config, mc *metrics.Collector) *Server {
	s := &Server{
		cfg:     cfg,
		mc:      mc,
		reg:     newRegistry(cfg, mc),
		blockCh: make(chan struct{}),
	}
	return s
}

// Start runs the HTTP server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	h := &handler{reg: s.reg, srv: s}
	srv := &http.Server{
		Addr:    s.cfg.MerkleServiceAddr,
		Handler: h,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("merkle service listening", "addr", s.cfg.MerkleServiceAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

// onBlockComplete is called (in a goroutine) by the handler when the block is
// full.  It drives the BUMP assembly and delivery pipeline.
func (s *Server) onBlockComplete(evt *BlockFinalizedEvent) {
	slog.Info("block finalizing",
		"subtrees", len(evt.SubtreeRoots),
		"tokens", len(evt.Callbacks),
	)

	processBUMPs(
		context.Background(),
		s.cfg.BlockHeight,
		s.cfg.SubtreeHeight(),
		s.cfg.TopTreeHeight(),
		s.cfg.BlockMerkleHeight(),
		s.mc,
		evt,
		s.cfg.CallbackURL(),
	)

	slog.Info("block pipeline complete")
	close(s.blockCh)
}
