package config

import "time"

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
}

// Default returns a Config with the standard test parameters.
//
//	HASHES_PER_BLOCK   = 61_440  (60 subtrees × 1024)
//	HASHES_PER_SUBTREE = 1_024
//	test duration       = 10 min → ~102.4 txids/sec
func Default() *Config {
	return &Config{
		HashesPerBlock:    61_440,
		HashesPerSubtree:  1_024,
		NumMiners:         3,
		NumBusinesses:     100,
		MerkleServiceAddr: ":8080",
		CallbackAddr:      ":3000",
		BlockHeight:       800_000,
		TestDuration:      10 * time.Minute,
		JitterPercent:     []float64{0.0, 0.05, 0.10},
	}
}

// NumSubtrees returns the number of subtrees per block.
func (c *Config) NumSubtrees() int { return c.HashesPerBlock / c.HashesPerSubtree }

// SubtreeHeight returns the number of merkle levels inside a single subtree.
// For HashesPerSubtree=1024 this is 10.
func (c *Config) SubtreeHeight() int { return log2Ceil(c.HashesPerSubtree) }

// TopTreeHeight returns the number of merkle levels in the "top tree" that
// combines all subtree roots into the block merkle root.
// For NumSubtrees=60 this is 6 (next power-of-2 ≥ 60 is 64 = 2^6).
func (c *Config) TopTreeHeight() int { return log2Ceil(c.NumSubtrees()) }

// BlockMerkleHeight returns the total height of the full block merkle tree.
// = SubtreeHeight + TopTreeHeight (= 16 for the defaults).
func (c *Config) BlockMerkleHeight() int { return c.SubtreeHeight() + c.TopTreeHeight() }

// SubmissionInterval returns the inter-submission delay to hit the correct rate.
func (c *Config) SubmissionInterval() time.Duration {
	return time.Duration(float64(c.TestDuration) / float64(c.HashesPerBlock))
}

// CallbackURL returns the base URL of the callback receiver.
func (c *Config) CallbackURL() string { return "http://localhost" + c.CallbackAddr }

// log2Ceil returns ⌈log₂(n)⌉ — i.e. the smallest h such that 2^h ≥ n.
func log2Ceil(n int) int {
	h := 0
	for (1 << h) < n {
		h++
	}
	return h
}
