// STUMPT – Subtree + BUMP Timing harness.
//
// Runs three components in a single process, communicating over localhost HTTP:
//
//  1. Callback server  (:13000) – receives BUMP deliveries from the merkle service.
//  2. Merkle service   (:18080) – accepts /watch, seals subtrees, assembles BUMPs.
//  3. Generator              – submits random txids to /watch at a controlled rate.
//
// With -direct mode, the generator bypasses HTTP and calls the registry directly,
// enabling large-scale runs (6M+ txids) that would be bottlenecked by HTTP.
//
// Usage:
//
//	./harness [flags]
//
// Flags:
//
//	-hashes-per-block   N    default 1024
//	-hashes-per-subtree N    default 64  (must divide hashes-per-block)
//	-miners             N    default 3
//	-businesses         N    default 100  (set to hashes-per-block for one BUMP per txid)
//	-duration           dur  default 10s  (controls txid submission rate; ignored in -direct mode)
//	-merkle-addr        addr default :18080
//	-callback-addr      addr default :13000
//	-direct                  bypass HTTP for txid submission (fast path for large-scale runs)
//	-dump-bump          path write first assembled BUMP as hex to this file
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/callback"
	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/generator"
	"github.com/bsv-blockchain/stumpt/internal/merkleservice"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
	"github.com/bsv-blockchain/stumpt/internal/netutil"
)

func main() {
	cfg := config.Default()
	var directMode bool

	flag.IntVar(&cfg.HashesPerBlock, "hashes-per-block", cfg.HashesPerBlock,
		"Total txids per simulated block")
	flag.IntVar(&cfg.HashesPerSubtree, "hashes-per-subtree", cfg.HashesPerSubtree,
		"Txids per subtree (must divide hashes-per-block)")
	flag.IntVar(&cfg.NumMiners, "miners", cfg.NumMiners,
		"Number of competing miners to simulate")
	flag.IntVar(&cfg.NumBusinesses, "businesses", cfg.NumBusinesses,
		"Number of distinct callback tokens (set equal to hashes-per-block for one BUMP per txid)")
	flag.DurationVar(&cfg.TestDuration, "duration", cfg.TestDuration,
		"Total test duration (controls submission rate; ignored in -direct mode)")
	flag.StringVar(&cfg.MerkleServiceAddr, "merkle-addr", cfg.MerkleServiceAddr,
		"Merkle service listen address")
	flag.StringVar(&cfg.CallbackAddr, "callback-addr", cfg.CallbackAddr,
		"Callback server listen address")
	flag.StringVar(&cfg.DumpBUMPFile, "dump-bump", cfg.DumpBUMPFile,
		"If set, write the first assembled BUMP as a hex string to this file")
	flag.BoolVar(&directMode, "direct", false,
		"Bypass HTTP for txid submission (fast path for large-scale runs)")
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
		"directMode", directMode,
	)

	mc := metrics.NewCollector()

	if directMode {
		runDirect(cfg, mc)
	} else {
		runHTTP(cfg, mc)
	}
}

// runDirect runs the harness in direct mode — no HTTP, no ticker pacing.
// Txids are submitted directly to the registry for maximum throughput.
func runDirect(cfg *config.Config, mc *metrics.Collector) {
	// In direct mode we still need the callback server for BUMP delivery.
	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Minute,
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

	// Bind callback listener.
	cbLn, cbAddr, err := netutil.Listen(cfg.CallbackAddr, 100)
	if err != nil {
		slog.Error("callback: bind failed", "err", err)
		os.Exit(1)
	}
	if cbAddr != cfg.CallbackAddr {
		slog.Info("callback port in use, shifted", "requested", cfg.CallbackAddr, "using", cbAddr)
	}
	cfg.CallbackAddr = cbAddr

	// Bind merkle service listener (needed for CallbackURL computation).
	msLn, msAddr, err := netutil.Listen(cfg.MerkleServiceAddr, 100)
	if err != nil {
		slog.Error("merkle service: bind failed", "err", err)
		os.Exit(1)
	}
	if msAddr != cfg.MerkleServiceAddr {
		slog.Info("merkle service port in use, shifted", "requested", cfg.MerkleServiceAddr, "using", msAddr)
	}
	cfg.MerkleServiceAddr = msAddr
	_ = msLn.Close() // Not needed in direct mode.

	// Start callback server.
	cbSrv := callback.NewServer(cbLn, mc)
	go cbSrv.Start(ctx)

	// Create the merkle service server to get access to the registry's
	// AddTxIDDirect method and the onBlockComplete pipeline.
	msSrv := merkleservice.NewServer(cfg, mc, nil)

	// Run the direct generator.
	gen := generator.NewDirect(cfg, mc)

	slog.Info("running in DIRECT mode (no HTTP submission)")

	cbURL := cfg.CallbackURL()
	evt := gen.RunDirect(func(txid chainhash.Hash, token string, _ string) interface{} {
		return msSrv.AddTxIDDirect(txid, token, cbURL)
	})

	if evt != nil {
		slog.Info("block complete in direct mode, processing BUMPs")
		msSrv.ProcessBlock(evt)
	}

	// Wait briefly for deliveries to flush.
	time.Sleep(2 * time.Second)

	mc.PrintSummary(cfg.NumBusinesses)
}

// runHTTP runs the original HTTP-based harness.
func runHTTP(cfg *config.Config, mc *metrics.Collector) {
	// The main context governs HTTP servers and the generator.
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

	// Cancel the main context as soon as the block pipeline finishes.
	go func() {
		msSrv.WaitForBlock(ctx)
		cancel()
	}()

	gen.Run(ctx)

	// ── Wait for block finalization + BUMP delivery ───────────────────────────
	slog.Info("generator done; waiting for block pipeline")
	msSrv.WaitForBlock(ctx)

	// ── Summary ───────────────────────────────────────────────────────────────
	mc.PrintSummary(cfg.NumBusinesses)
}

// formatRate returns a human-readable rate string.
func formatRate(count int, d time.Duration) string {
	if d == 0 {
		return "∞"
	}
	return fmt.Sprintf("%.0f/s", float64(count)/d.Seconds())
}
