// STUMPT – Subtree + BUMP Timing harness.
//
// Runs a 4-phase pipeline in a single process:
//
//  1. Generate random txids (Phase 1)
//  2. Seal subtrees to disk for all miners (Phase 2)
//  3. Token→position indexing (done during Phase 2)
//  4. Block found: load from disk, assemble BUMPs (Phase 4 — critical path)
//
// Usage:
//
//	./harness [flags]
//
// Flags:
//
//	-hashes-per-block   N    default auto-detected from RAM
//	-hashes-per-subtree N    default 1048576 (1M leaves per subtree)
//	-miners             N    default 3
//	-businesses         N    default 1000
//	-dump-bump          path write first assembled BUMP as hex to this file
//	-data-dir           path BadgerDB data directory (empty = temp dir)
//	-max-memory         GB   peak memory budget in GB (default: 55% of system RAM)
//	-requirements            print system requirements table and exit
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/config"
	"github.com/bsv-blockchain/stumpt/internal/diskstore"
	"github.com/bsv-blockchain/stumpt/internal/merkleservice"
	"github.com/bsv-blockchain/stumpt/internal/metrics"
)

func main() {
	cfg := config.Default()
	var showRequirements bool
	var maxMemGB float64

	flag.IntVar(&cfg.HashesPerBlock, "hashes-per-block", cfg.HashesPerBlock,
		"Total txids per simulated block (default: auto-detected from RAM)")
	flag.IntVar(&cfg.HashesPerSubtree, "hashes-per-subtree", cfg.HashesPerSubtree,
		"Txids per subtree (must divide hashes-per-block)")
	flag.IntVar(&cfg.NumMiners, "miners", cfg.NumMiners,
		"Number of competing miners to simulate")
	flag.IntVar(&cfg.NumBusinesses, "businesses", cfg.NumBusinesses,
		"Number of distinct callback tokens")
	flag.StringVar(&cfg.DumpBUMPFile, "dump-bump", cfg.DumpBUMPFile,
		"If set, write the first assembled BUMP as a hex string to this file")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir,
		"BadgerDB data directory (empty = temp dir)")
	flag.Float64Var(&maxMemGB, "max-memory", 0,
		"Peak memory budget in GB (default: 55% of system RAM)")
	flag.BoolVar(&showRequirements, "requirements", false,
		"Print system requirements table and exit")
	flag.Parse()

	if showRequirements {
		config.PrintSystemRequirements()
		os.Exit(0)
	}

	// Apply max-memory override: recalculate HashesPerBlock if the user didn't
	// explicitly set it, so the auto-detected scale fits the new budget.
	hashesExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "hashes-per-block" {
			hashesExplicit = true
		}
	})
	if maxMemGB > 0 {
		cfg.MaxMemoryBytes = int64(maxMemGB * float64(1<<30))
		if !hashesExplicit {
			cfg = config.WithMemoryBudget(cfg.MaxMemoryBytes)
		}
	}

	// Validate.
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

	// Check memory budget before proceeding.
	if err := cfg.CheckMemory(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		config.PrintSystemRequirements()
		os.Exit(1)
	}

	// Tell Go's GC to stay within our memory budget. This makes the GC
	// trigger more aggressively as the heap approaches MaxMemoryBytes,
	// preventing the RSS from blowing past our estimate.
	debug.SetMemoryLimit(cfg.MaxMemoryBytes)

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
		"estMemoryGB", fmt.Sprintf("%.1f", float64(cfg.EstimatedMemoryBytes())/(1<<30)),
		"memBudgetGB", fmt.Sprintf("%.1f", float64(cfg.MaxMemoryBytes)/(1<<30)),
		"memLimitGB", fmt.Sprintf("%.1f", float64(cfg.MaxMemoryBytes)/(1<<30)),
	)

	// Handle OS signals for clean shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("interrupt received, exiting")
		os.Exit(1)
	}()

	mc := metrics.NewCollector()
	run(cfg, mc)
}

func run(cfg *config.Config, mc *metrics.Collector) {
	// Open BadgerDB for subtree storage.
	db, err := diskstore.Open(cfg.DataDir)
	if err != nil {
		slog.Error("badgerdb: open failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	overheadBytes := config.OverheadBytes()

	// ════════════════════════════════════════════════════════════════════════
	// PHASE 1 — Generate txids
	// ════════════════════════════════════════════════════════════════════════
	slog.Info("PHASE 1: generating txids", "count", cfg.HashesPerBlock)
	t1 := time.Now()

	txids := make([]chainhash.Hash, cfg.HashesPerBlock)
	// Generate in parallel chunks for speed.
	numWorkers := runtime.GOMAXPROCS(0)
	chunkSize := (cfg.HashesPerBlock + numWorkers - 1) / numWorkers
	var genWg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		lo := w * chunkSize
		hi := lo + chunkSize
		if hi > cfg.HashesPerBlock {
			hi = cfg.HashesPerBlock
		}
		if lo >= hi {
			break
		}
		genWg.Add(1)
		go func(lo, hi int) {
			defer genWg.Done()
			for i := lo; i < hi; i++ {
				rand.Read(txids[i][:])
			}
		}(lo, hi)
	}
	genWg.Wait()

	phase1Dur := time.Since(t1)
	mc.RecordPhase1(phase1Dur)
	slog.Info("PHASE 1 complete",
		"txids", cfg.HashesPerBlock,
		"duration", phase1Dur,
		"rate", fmt.Sprintf("%.0f/s", float64(cfg.HashesPerBlock)/phase1Dur.Seconds()),
	)

	// Remember first txid for coinbase replacement.
	firstTxid := txids[0]

	// ════════════════════════════════════════════════════════════════════════
	// PHASE 2 — Seal subtrees to disk + index token positions
	// ════════════════════════════════════════════════════════════════════════
	slog.Info("PHASE 2: sealing subtrees to disk",
		"numSubtrees", cfg.NumSubtrees(),
		"miners", cfg.NumMiners,
	)
	t2 := time.Now()

	// Per-miner token→subtree position indexes.
	minerTokenIdx := make([]*merkleservice.TokenSubtreeIndex, cfg.NumMiners)
	for i := range minerTokenIdx {
		minerTokenIdx[i] = merkleservice.NewTokenSubtreeIndex()
	}

	minerRoots := merkleservice.SealSubtreesToDisk(txids, cfg, db, mc, minerTokenIdx)

	phase2Dur := time.Since(t2)
	mc.RecordPhase2(phase2Dur)
	slog.Info("PHASE 2 complete",
		"subtrees", cfg.NumSubtrees(),
		"duration", phase2Dur,
	)

	// Free the txid list — it's no longer needed.
	txids = nil
	runtime.GC()
	// Force the OS to reclaim freed pages. Without this, macOS MADV_FREE
	// keeps freed pages as RSS until under memory pressure, which causes
	// Phase 4 cache allocations to stack on top of Phase 2's ghost RSS.
	debug.FreeOSMemory()

	// ════════════════════════════════════════════════════════════════════════
	// PHASE 4 — Block found (critical path, timed)
	// ════════════════════════════════════════════════════════════════════════
	slog.Info("PHASE 4: block found — assembling BUMPs")
	t4 := time.Now()

	// Finalize: pick winner, coinbase reseal.
	evt := merkleservice.FinalizeBlock(cfg, mc, db, minerRoots, minerTokenIdx, firstTxid)
	if evt == nil {
		slog.Error("block finalization failed")
		os.Exit(1)
	}

	// Free non-winner TokenSubtreeIndex data.
	for m := 0; m < cfg.NumMiners; m++ {
		if m != evt.WinnerMiner {
			minerTokenIdx[m] = nil
		}
	}
	minerTokenIdx = nil
	minerRoots = nil
	runtime.GC()
	debug.FreeOSMemory()

	// Calculate subtree cache size conservatively.
	//
	// Each cached subtree holds leaves + store in memory:
	//   leaves: HashesPerSubtree × 32B
	//   store:  ~HashesPerSubtree × 32B (NextPowerOfTwo(N)-1 nodes)
	//   Total:  ~66B per leaf
	//
	// Available = budget - overhead - winner's TokenSubtreeIndex - GC residual - BUMP workers
	bytesPerCachedSubtree := int64(cfg.HashesPerSubtree) * 66
	tokenIdxMem := int64(cfg.HashesPerBlock) * 10             // winner's index with map overhead
	gcResidual := int64(2 << 30)                              // GC may retain ~2GB of freed Phase 2 data
	bumpWorkerMem := int64(runtime.GOMAXPROCS(0)) * 50 << 20  // ~50MB per BUMP worker
	available := cfg.MaxMemoryBytes - overheadBytes - tokenIdxMem - gcResidual - bumpWorkerMem
	if available < bytesPerCachedSubtree {
		available = bytesPerCachedSubtree // at least 1 entry
	}
	maxCacheEntries := int(available / bytesPerCachedSubtree)
	if maxCacheEntries < 1 {
		maxCacheEntries = 1
	}
	if maxCacheEntries > cfg.NumSubtrees() {
		maxCacheEntries = cfg.NumSubtrees()
	}

	slog.Info("subtree cache configured",
		"maxEntries", maxCacheEntries,
		"bytesPerSubtree", bytesPerCachedSubtree,
		"availableGB", fmt.Sprintf("%.1f", float64(available)/(1<<30)),
	)

	// Assemble BUMPs from disk-backed subtrees.
	merkleservice.ProcessBUMPs(
		cfg.BlockHeight,
		cfg.SubtreeHeight(),
		cfg.TopTreeHeight(),
		cfg.BlockMerkleHeight(),
		mc,
		evt,
		cfg.DumpBUMPFile,
		maxCacheEntries,
	)

	phase4Dur := time.Since(t4)
	mc.RecordPhase4(phase4Dur)
	slog.Info("PHASE 4 complete",
		"duration", phase4Dur,
	)

	// ════════════════════════════════════════════════════════════════════════
	// Summary
	// ════════════════════════════════════════════════════════════════════════
	mc.PrintSummary(cfg.HashesPerBlock, cfg.NumBusinesses)
}
