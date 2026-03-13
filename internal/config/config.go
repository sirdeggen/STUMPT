package config

import (
	"fmt"
	"net"
	"time"
)

// bytesPerTxid is the estimated in-memory cost per txid:
//   - Miner-0 subtree leaves+store: 64B
//   - In-memory txidIndex (Go map): ~90B
//   - TokenSubtreeIndex: ~4B
const bytesPerTxid = 158

// bumpOverheadBytes is the estimated fixed overhead for BUMP assembly,
// BadgerDB, Go runtime, and other per-process costs.
const bumpOverheadBytes = 4 << 30 // 4 GB

// Config holds all configurable parameters for the STUMPT test harness.
type Config struct {
	// HashesPerBlock is the total number of txids in one simulated block.
	HashesPerBlock int
	// HashesPerSubtree is the number of txids that form one subtree.
	// Must divide HashesPerBlock evenly.
	HashesPerSubtree int
	// NumMiners is the number of competing miners to simulate.
	NumMiners int
	// NumBusinesses is the number of distinct callback tokens (submitters).
	NumBusinesses int
	// MerkleServiceAddr is the listen address for the merkle service HTTP server.
	MerkleServiceAddr string
	// CallbackAddr is the listen address for the mock callback receiver.
	CallbackAddr string
	// BlockHeight is the mock block height stamped in every BUMP.
	BlockHeight uint32
	// TestDuration is the total duration over which txids are submitted.
	TestDuration time.Duration
	// JitterPercent defines the fraction of adjacent pairs swapped per miner.
	// len must equal NumMiners; index 0 is always 0.0 (canonical order).
	JitterPercent []float64

	// DumpBUMPFile is an optional file path.  When non-empty the first
	// assembled compound BUMP is written as a UTF-8 hex string to this file.
	DumpBUMPFile string

	// DataDir is the directory for BadgerDB on-disk storage.
	// When empty, a temporary directory is created and removed on close.
	DataDir string

	// MaxMemoryBytes is the peak memory budget. When 0, auto-detected from
	// the system's physical RAM (80% of total).
	MaxMemoryBytes int64
}

// Default returns a Config optimized for the current machine.
// It auto-detects available memory and sets HashesPerBlock to use
// 1M-leaf subtrees filling ~80% of physical RAM.
func Default() *Config {
	sysMem := systemMemory()
	maxMem := int64(float64(sysMem) * 0.80)
	if maxMem < 2<<30 {
		maxMem = 2 << 30 // minimum 2 GB
	}

	// Calculate how many 1M-leaf subtrees fit in the memory budget.
	hashesPerSubtree := 1_048_576
	availableForData := maxMem - bumpOverheadBytes
	if availableForData < 0 {
		availableForData = int64(hashesPerSubtree) * bytesPerTxid // at least 1 subtree
	}
	maxTxids := availableForData / bytesPerTxid
	numSubtrees := int(maxTxids) / hashesPerSubtree
	if numSubtrees < 2 {
		numSubtrees = 2
	}
	// Round down to a nice number for cleaner output.
	if numSubtrees > 128 {
		numSubtrees = (numSubtrees / 16) * 16
	} else if numSubtrees > 16 {
		numSubtrees = (numSubtrees / 4) * 4
	}

	return &Config{
		HashesPerBlock:    numSubtrees * hashesPerSubtree,
		HashesPerSubtree:  hashesPerSubtree,
		NumMiners:         3,
		NumBusinesses:     1000,
		MerkleServiceAddr: ":18080",
		CallbackAddr:      ":13000",
		BlockHeight:       800_000,
		TestDuration:      10 * time.Second,
		JitterPercent:     []float64{0.0, 0.05, 0.10},
		MaxMemoryBytes:    maxMem,
	}
}

// WithMemoryBudget returns a Config like Default() but scaled to fit maxMem.
func WithMemoryBudget(maxMem int64) *Config {
	cfg := Default()
	cfg.MaxMemoryBytes = maxMem
	if maxMem < 2<<30 {
		maxMem = 2 << 30
	}

	hashesPerSubtree := cfg.HashesPerSubtree
	availableForData := maxMem - bumpOverheadBytes
	if availableForData < 0 {
		availableForData = int64(hashesPerSubtree) * bytesPerTxid
	}
	maxTxids := availableForData / bytesPerTxid
	numSubtrees := int(maxTxids) / hashesPerSubtree
	if numSubtrees < 2 {
		numSubtrees = 2
	}
	if numSubtrees > 128 {
		numSubtrees = (numSubtrees / 16) * 16
	} else if numSubtrees > 16 {
		numSubtrees = (numSubtrees / 4) * 4
	}

	cfg.HashesPerBlock = numSubtrees * hashesPerSubtree
	return cfg
}

// NumSubtrees returns the number of subtrees per block.
func (c *Config) NumSubtrees() int { return c.HashesPerBlock / c.HashesPerSubtree }

// SubtreeHeight returns the number of merkle levels inside a single subtree.
func (c *Config) SubtreeHeight() int { return log2Ceil(c.HashesPerSubtree) }

// TopTreeHeight returns the number of merkle levels in the "top tree" that
// combines all subtree roots into the block merkle root.
func (c *Config) TopTreeHeight() int { return log2Ceil(c.NumSubtrees()) }

// BlockMerkleHeight returns the total height of the full block merkle tree.
func (c *Config) BlockMerkleHeight() int { return c.SubtreeHeight() + c.TopTreeHeight() }

// SubmissionInterval returns the inter-submission delay to hit the correct rate.
func (c *Config) SubmissionInterval() time.Duration {
	return time.Duration(float64(c.TestDuration) / float64(c.HashesPerBlock))
}

// EstimatedMemoryBytes returns the estimated peak memory usage for this config.
func (c *Config) EstimatedMemoryBytes() int64 {
	return int64(c.HashesPerBlock)*bytesPerTxid + bumpOverheadBytes
}

// CheckMemory returns an error if the estimated memory exceeds the budget.
func (c *Config) CheckMemory() error {
	est := c.EstimatedMemoryBytes()
	limit := c.MaxMemoryBytes
	if limit <= 0 {
		limit = int64(float64(systemMemory()) * 0.80)
	}
	if est > limit {
		return fmt.Errorf(
			"estimated memory %.1f GB exceeds budget %.1f GB (%.0fM txids × %d B/txid + %.1f GB overhead)\n"+
				"Reduce -hashes-per-block or -hashes-per-subtree, or increase -max-memory",
			float64(est)/(1<<30),
			float64(limit)/(1<<30),
			float64(c.HashesPerBlock)/1e6,
			bytesPerTxid,
			float64(bumpOverheadBytes)/(1<<30),
		)
	}
	return nil
}

// PrintSystemRequirements prints a table of memory requirements for various scales.
func PrintSystemRequirements() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                     STUMPT System Requirements                          ║")
	fmt.Println("╠═══════════════╦═══════════════╦═══════════════╦══════════════════════════╣")
	fmt.Println("║   Total TXIDs ║   Subtrees    ║   Est. RAM    ║   Min System RAM        ║")
	fmt.Println("╠═══════════════╬═══════════════╬═══════════════╬══════════════════════════╣")

	cases := []struct {
		subtrees int
		label    string
	}{
		{2, "2 × 1M"},
		{4, "4 × 1M"},
		{10, "10 × 1M"},
		{20, "20 × 1M"},
		{40, "40 × 1M"},
		{60, "60 × 1M"},
		{100, "100 × 1M"},
		{150, "150 × 1M"},
		{300, "300 × 1M"},
		{600, "600 × 1M"},
	}
	hps := 1_048_576
	for _, c := range cases {
		txids := c.subtrees * hps
		est := int64(txids)*bytesPerTxid + bumpOverheadBytes
		minSys := float64(est) / 0.80
		fmt.Printf("║  %12s ║  %11s ║  %9.1f GB ║  %9.1f GB             ║\n",
			fmtCount(txids), c.label, float64(est)/(1<<30), minSys/(1<<30))
	}
	fmt.Println("╚═══════════════╩═══════════════╩═══════════════╩══════════════════════════╝")
	fmt.Println()
	fmt.Printf("Memory model: %d B/txid (miner-0 subtrees + txidIndex + tokenSubtreeIndex) + %.0f GB overhead\n",
		bytesPerTxid, float64(bumpOverheadBytes)/(1<<30))
	fmt.Println()
}

func fmtCount(n int) string {
	if n >= 1_000_000_000 {
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	}
	return fmt.Sprintf("%d", n)
}

// CallbackURL returns the base URL of the callback receiver.
func (c *Config) CallbackURL() string {
	_, port, err := net.SplitHostPort(c.CallbackAddr)
	if err != nil {
		return "http://localhost" + c.CallbackAddr
	}
	return "http://localhost:" + port
}

// log2Ceil returns ⌈log₂(n)⌉ — i.e. the smallest h such that 2^h ≥ n.
func log2Ceil(n int) int {
	h := 0
	for (1 << h) < n {
		h++
	}
	return h
}
