// STUMPT – Subtree + BUMP Timing harness.
//
// Runs three components in a single process:
//
//  1. Callback server  (:13000) – receives BUMP deliveries from the merkle service.
//  2. Merkle service   (:18080) – accepts /watch, seals subtrees, assembles BUMPs.
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
//	-businesses         N    default 100  (set to hashes-per-block for one BUMP per txid)
//	-duration           dur  default 10s  (controls txid submission rate)
//	-merkle-addr        addr default :18080
//	-callback-addr      addr default :13000
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
	"github.com/bsv-blockchain/stumpt/internal/netutil"
)

func main() {
	cfg := config.Default()

	flag.IntVar(&cfg.HashesPerBlock, "hashes-per-block", cfg.HashesPerBlock,
		"Total txids per simulated block")
	flag.IntVar(&cfg.HashesPerSubtree, "hashes-per-subtree", cfg.HashesPerSubtree,
		"Txids per subtree (must divide hashes-per-block)")
	flag.IntVar(&cfg.NumMiners, "miners", cfg.NumMiners,
		"Number of competing miners to simulate")
	flag.IntVar(&cfg.NumBusinesses, "businesses", cfg.NumBusinesses,
		"Number of distinct callback tokens (set equal to hashes-per-block for one BUMP per txid)")
	flag.DurationVar(&cfg.TestDuration, "duration", cfg.TestDuration,
		"Total test duration (controls submission rate)")
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

	// The main context governs HTTP servers and the generator.  We give it
	// a large ceiling (TestDuration × 2 + 5 min) so that a run which
	// overshoots its target duration (e.g. OS scheduling jitter at high
	// submission rates) is not cut off before block finalisation.
	// BUMP delivery uses its own independent context (see server.go) and is
	// never subject to this deadline.
	ctx, cancel := context.WithTimeout(
		context.Background(),
		cfg.TestDuration*2+5*time.Minute,
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

	// ── Bind listeners (auto-increment port if in use) ───────────────────────
	cbLn, cbAddr, err := netutil.Listen(cfg.CallbackAddr, 100)
	if err != nil {
		slog.Error("callback: bind failed", "err", err)
		os.Exit(1)
	}
	if cbAddr != cfg.CallbackAddr {
		slog.Info("callback port in use, shifted", "requested", cfg.CallbackAddr, "using", cbAddr)
	}
	cfg.CallbackAddr = cbAddr

	msLn, msAddr, err := netutil.Listen(cfg.MerkleServiceAddr, 100)
	if err != nil {
		slog.Error("merkle service: bind failed", "err", err)
		os.Exit(1)
	}
	if msAddr != cfg.MerkleServiceAddr {
		slog.Info("merkle service port in use, shifted", "requested", cfg.MerkleServiceAddr, "using", msAddr)
	}
	cfg.MerkleServiceAddr = msAddr

	// ── Start callback server ─────────────────────────────────────────────────
	cbSrv := callback.NewServer(cbLn, mc)
	go cbSrv.Start(ctx)

	// ── Start merkle service ──────────────────────────────────────────────────
	msSrv := merkleservice.NewServer(cfg, mc, msLn)
	go msSrv.Start(ctx)

	// ── Run generator (blocks until all txids submitted) ─────────────────────
	gen := generator.New(cfg, mc)

	// Cancel the main context as soon as the block pipeline finishes so the
	// generator and all other goroutines exit promptly.
	go func() {
		msSrv.WaitForBlock(ctx)
		cancel()
	}()

	gen.Run(ctx)

	// ── Wait for block finalization + BUMP delivery ───────────────────────────
	// (WaitForBlock returns immediately if already done, or waits with main ctx.)
	slog.Info("generator done; waiting for block pipeline")
	msSrv.WaitForBlock(ctx)

	// ── Summary ───────────────────────────────────────────────────────────────
	mc.PrintSummary(cfg.NumBusinesses)
}
