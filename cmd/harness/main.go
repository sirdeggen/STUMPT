// STUMPT – Subtree + BUMP Timing harness.
//
// Runs three components in a single process:
//
//  1. Callback server  (:3000) – receives BUMP deliveries from the merkle service.
//  2. Merkle service   (:8080) – accepts /watch, seals subtrees, assembles BUMPs.
//  3. Generator              – submits random txids to /watch at a controlled rate.
//
// Usage:
//
//	./harness [flags]
//
// Flags:
//
//	-hashes-per-block   N    default 61440
//	-hashes-per-subtree N    default 1024  (must divide hashes-per-block)
//	-miners             N    default 3
//	-merkle-addr        addr default :8080
//	-callback-addr      addr default :3000
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bsv-blockchain/stumpt/internal/callback"
	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/generator"
	"github.com/bsv-blockchain/stumpt/internal/merkleservice"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
)

func main() {
	cfg := config.Default()

	flag.IntVar(&cfg.HashesPerBlock, "hashes-per-block", cfg.HashesPerBlock,
		"Total txids per simulated block")
	flag.IntVar(&cfg.HashesPerSubtree, "hashes-per-subtree", cfg.HashesPerSubtree,
		"Txids per subtree (must divide hashes-per-block)")
	flag.IntVar(&cfg.NumMiners, "miners", cfg.NumMiners,
		"Number of competing miners to simulate")
	flag.StringVar(&cfg.MerkleServiceAddr, "merkle-addr", cfg.MerkleServiceAddr,
		"Merkle service listen address")
	flag.StringVar(&cfg.CallbackAddr, "callback-addr", cfg.CallbackAddr,
		"Callback server listen address")
	flag.Parse()

	// Validate
	if cfg.HashesPerBlock <= 0 || cfg.HashesPerSubtree <= 0 {
		slog.Error("hashes-per-block and hashes-per-subtree must be positive")
		os.Exit(1)
	}
	if cfg.HashesPerBlock%cfg.HashesPerSubtree != 0 {
		slog.Error("hashes-per-block must be divisible by hashes-per-subtree",
			"hashes-per-block", cfg.HashesPerBlock,
			"hashes-per-subtree", cfg.HashesPerSubtree,
		)
		os.Exit(1)
	}

	// Extend JitterPercent if NumMiners was changed via flag.
	for len(cfg.JitterPercent) < cfg.NumMiners {
		cfg.JitterPercent = append(cfg.JitterPercent, 0.10)
	}
	cfg.JitterPercent = cfg.JitterPercent[:cfg.NumMiners]

	// Structured JSON logging.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	slog.Info("STUMPT starting",
		"hashesPerBlock", cfg.HashesPerBlock,
		"hashesPerSubtree", cfg.HashesPerSubtree,
		"numSubtrees", cfg.NumSubtrees(),
		"subtreeHeight", cfg.SubtreeHeight(),
		"topTreeHeight", cfg.TopTreeHeight(),
		"blockMerkleHeight", cfg.BlockMerkleHeight(),
		"numMiners", cfg.NumMiners,
		"numBusinesses", cfg.NumBusinesses,
		"submissionInterval", cfg.SubmissionInterval(),
		"testDuration", cfg.TestDuration,
	)

	mc := metrics.NewCollector()

	// Context: test duration + generous buffer for BUMP delivery.
	ctx, cancel := context.WithTimeout(
		context.Background(),
		cfg.TestDuration+2*time.Minute,
	)
	defer cancel()

	// Handle OS signals for clean shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("interrupt received, shutting down")
		cancel()
	}()

	// ── Start callback server ─────────────────────────────────────────────────
	cbSrv := callback.NewServer(cfg.CallbackAddr, mc)
	go cbSrv.Start(ctx)
	time.Sleep(80 * time.Millisecond) // give it a moment to bind

	// ── Start merkle service ──────────────────────────────────────────────────
	msSrv := merkleservice.NewServer(cfg, mc)
	go msSrv.Start(ctx)
	time.Sleep(80 * time.Millisecond) // give it a moment to bind

	// ── Run generator (blocks until all txids submitted) ─────────────────────
	gen := generator.New(cfg, mc)
	gen.Run(ctx)

	// ── Wait for block finalization + BUMP delivery ───────────────────────────
	slog.Info("generator done; waiting for block pipeline")
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer waitCancel()
	msSrv.WaitForBlock(waitCtx)

	// ── Summary ───────────────────────────────────────────────────────────────
	mc.PrintSummary()
}
