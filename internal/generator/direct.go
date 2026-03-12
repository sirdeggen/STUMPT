package generator

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
)

// TxIDReceiver is the interface that AddTxIDDirect satisfies, allowing the
// direct generator to call the registry without HTTP.
type TxIDReceiver interface {
	AddTxIDDirect(txid chainhash.Hash, token string, cb CallbackInfo) interface{}
}

// CallbackInfo mirrors merkleservice.CallbackInfo to avoid import cycles.
type CallbackInfo struct {
	URL   string
	Token string
}

// DirectGenerator bypasses HTTP and submits txids directly to the registry.
// This eliminates the HTTP/JSON overhead that becomes the bottleneck at
// large scale (>60k txids), enabling benchmarking of the merkle/BUMP
// pipeline in isolation.
type DirectGenerator struct {
	cfg    *config.Config
	mc     *metrics.Collector
	doneCh chan struct{}
}

// NewDirect creates a DirectGenerator.
func NewDirect(cfg *config.Config, mc *metrics.Collector) *DirectGenerator {
	return &DirectGenerator{
		cfg:    cfg,
		mc:     mc,
		doneCh: make(chan struct{}),
	}
}

// Done returns a channel that is closed when all txids have been submitted.
func (g *DirectGenerator) Done() <-chan struct{} { return g.doneCh }

// RunDirect is a generic version that accepts any function with the right
// signature. It generates all txids and submits them directly, measuring
// pure submission throughput without HTTP overhead.
//
// The addFn should be registry.AddTxIDDirect (or a compatible function).
// It returns the BlockFinalizedEvent if one was produced.
//
// This method does NOT pace submissions — it runs as fast as possible,
// which is the desired behavior for benchmarking the merkle pipeline.
func (g *DirectGenerator) RunDirect(addFn func(txid chainhash.Hash, token string, cbURL string) interface{}) interface{} {
	defer close(g.doneCh)

	cfg := g.cfg
	cbURL := cfg.CallbackURL()

	slog.Info("direct generator starting",
		"hashes", cfg.HashesPerBlock,
		"businesses", cfg.NumBusinesses,
		"mode", "direct (no HTTP)",
	)

	t0 := time.Now()
	var lastEvt interface{}

	for i := 0; i < cfg.HashesPerBlock; i++ {
		var txid chainhash.Hash
		if _, err := rand.Read(txid[:]); err != nil {
			panic("crypto/rand read: " + err.Error())
		}

		token := fmt.Sprintf("token-%d", i%cfg.NumBusinesses)
		evt := addFn(txid, token, cbURL)
		if evt != nil {
			lastEvt = evt
		}

		g.mc.RecordSubmit()

		if (i+1)%10_000 == 0 {
			elapsed := time.Since(t0)
			rate := float64(i+1) / elapsed.Seconds()
			slog.Info("direct generator progress",
				"submitted", i+1,
				"remaining", cfg.HashesPerBlock-(i+1),
				"elapsed", elapsed.Round(time.Millisecond),
				"rate", fmt.Sprintf("%.0f txids/s", rate),
			)
		}
	}

	elapsed := time.Since(t0)
	rate := float64(cfg.HashesPerBlock) / elapsed.Seconds()
	slog.Info("direct generator finished",
		"total", cfg.HashesPerBlock,
		"elapsed", elapsed.Round(time.Millisecond),
		"rate", fmt.Sprintf("%.0f txids/s", rate),
	)

	return lastEvt
}
