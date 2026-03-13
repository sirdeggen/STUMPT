// Package metrics collects and reports timing data for the STUMPT harness.
package metrics

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Collector accumulates timing and count data during the test run.
type Collector struct {
	startTime time.Time

	// Phase 1 — generation
	phase1Ns atomic.Int64

	// Phase 2 — subtree sealing + disk writes
	phase2Ns           atomic.Int64
	subtreeSealCount   atomic.Int64
	subtreeSealTotalNs atomic.Int64
	diskWriteCount     atomic.Int64
	diskWriteTotalNs   atomic.Int64

	// Phase 2 also includes token indexing
	proofComputeCount   atomic.Int64
	proofComputeTotalNs atomic.Int64

	// Phase 4 — block found (critical path)
	phase4Ns         atomic.Int64
	coinbaseResealNs atomic.Int64
	topTreeNs        atomic.Int64
	bumpAssembly     atomic.Int64

	// Disk reads during BUMP assembly
	diskReadCount    atomic.Int64
	diskReadTotalNs  atomic.Int64
	cacheHits        atomic.Int64
	cacheMisses      atomic.Int64

	// BUMP results
	mu        sync.Mutex
	bumpSizes []int
	bumpCount atomic.Int64
	bumpBytes atomic.Int64
}

// NewCollector creates a new Collector and records the start time.
func NewCollector() *Collector {
	return &Collector{startTime: time.Now()}
}

// RecordPhase1 records Phase 1 (generation) duration.
func (c *Collector) RecordPhase1(d time.Duration) { c.phase1Ns.Store(d.Nanoseconds()) }

// RecordPhase2 records Phase 2 (sealing) total duration.
func (c *Collector) RecordPhase2(d time.Duration) { c.phase2Ns.Store(d.Nanoseconds()) }

// RecordPhase4 records Phase 4 (block found critical path) total duration.
func (c *Collector) RecordPhase4(d time.Duration) { c.phase4Ns.Store(d.Nanoseconds()) }

// RecordSubtreeSeal records how long it took to build all miners' subtrees.
func (c *Collector) RecordSubtreeSeal(d time.Duration) {
	c.subtreeSealCount.Add(1)
	c.subtreeSealTotalNs.Add(d.Nanoseconds())
}

// RecordDiskWrite records a disk write operation.
func (c *Collector) RecordDiskWrite(d time.Duration) {
	c.diskWriteCount.Add(1)
	c.diskWriteTotalNs.Add(d.Nanoseconds())
}

// RecordDiskRead records a disk read operation.
func (c *Collector) RecordDiskRead(d time.Duration) {
	c.diskReadCount.Add(1)
	c.diskReadTotalNs.Add(d.Nanoseconds())
}

// RecordCacheHit records a subtree cache hit during BUMP assembly.
func (c *Collector) RecordCacheHit() { c.cacheHits.Add(1) }

// RecordCacheMiss records a subtree cache miss during BUMP assembly.
func (c *Collector) RecordCacheMiss() { c.cacheMisses.Add(1) }

// RecordProofCompute records how long indexing token positions took for one subtree.
func (c *Collector) RecordProofCompute(d time.Duration) {
	c.proofComputeCount.Add(1)
	c.proofComputeTotalNs.Add(d.Nanoseconds())
}

// RecordCoinbaseReseal records how long the coinbase replacement + subtree-0
// re-seal took at block time.
func (c *Collector) RecordCoinbaseReseal(d time.Duration) { c.coinbaseResealNs.Store(d.Nanoseconds()) }

// RecordTopTreeBuild records the top-tree assembly duration at block time.
func (c *Collector) RecordTopTreeBuild(d time.Duration) { c.topTreeNs.Store(d.Nanoseconds()) }

// RecordBUMPAssembly records the total BUMP compound-assembly duration.
func (c *Collector) RecordBUMPAssembly(d time.Duration) { c.bumpAssembly.Store(d.Nanoseconds()) }

// BUMPCount returns the current number of assembled BUMPs.
func (c *Collector) BUMPCount() int64 { return c.bumpCount.Load() }

// RecordBUMP records a completed BUMP's size.
func (c *Collector) RecordBUMP(bytes int) {
	c.bumpCount.Add(1)
	c.bumpBytes.Add(int64(bytes))
	c.mu.Lock()
	c.bumpSizes = append(c.bumpSizes, bytes)
	c.mu.Unlock()
}

// PrintSummary writes the final human-readable report to stdout.
func (c *Collector) PrintSummary(totalTxids, numBusinesses int) {
	elapsed := time.Since(c.startTime)

	phase1Ms := float64(c.phase1Ns.Load()) / 1e6
	phase2Ms := float64(c.phase2Ns.Load()) / 1e6
	phase4Ms := float64(c.phase4Ns.Load()) / 1e6

	sealCount := c.subtreeSealCount.Load()
	avgSealMs := avgMs(c.subtreeSealTotalNs.Load(), sealCount)
	diskWrites := c.diskWriteCount.Load()
	avgDiskWriteMs := avgMs(c.diskWriteTotalNs.Load(), diskWrites)
	diskReads := c.diskReadCount.Load()
	avgDiskReadMs := avgMs(c.diskReadTotalNs.Load(), diskReads)
	cacheHits := c.cacheHits.Load()
	cacheMisses := c.cacheMisses.Load()

	coinbaseMs := float64(c.coinbaseResealNs.Load()) / 1e6
	topTreeMs := float64(c.topTreeNs.Load()) / 1e6
	bumpAssemblyMs := float64(c.bumpAssembly.Load()) / 1e6

	bumpCount := c.bumpCount.Load()
	bumpBytes := c.bumpBytes.Load()
	var avgBumpBytes int64
	if bumpCount > 0 {
		avgBumpBytes = bumpBytes / bumpCount
	}

	genRate := float64(totalTxids) / (float64(c.phase1Ns.Load()) / 1e9)

	fmt.Printf("\n╔══════════════════════════════════════════════╗\n")
	fmt.Printf("║            STUMPT FINAL SUMMARY              ║\n")
	fmt.Printf("╠══════════════════════════════════════════════╣\n")
	fmt.Printf("║  Total elapsed:              %13s  ║\n", elapsed.Round(time.Millisecond))
	fmt.Printf("║  Total txids:                %13d  ║\n", totalTxids)
	fmt.Printf("╠══════════════════════════════════════════════╣\n")
	fmt.Printf("║  PHASE 1 — Generation        %12.0fms  ║\n", phase1Ms)
	fmt.Printf("║    Rate:                   %12.0f/s  ║\n", genRate)
	fmt.Printf("╠══════════════════════════════════════════════╣\n")
	fmt.Printf("║  PHASE 2 — Seal + Index      %12.0fms  ║\n", phase2Ms)
	fmt.Printf("║    Subtrees sealed:          %13d  ║\n", sealCount)
	fmt.Printf("║    Avg seal time:            %12.2fms  ║\n", avgSealMs)
	fmt.Printf("║    Disk writes:              %13d  ║\n", diskWrites)
	fmt.Printf("║    Avg disk write:           %12.2fms  ║\n", avgDiskWriteMs)
	fmt.Printf("╠══════════════════════════════════════════════╣\n")
	fmt.Printf("║  PHASE 4 — Block Found       %12.0fms  ║\n", phase4Ms)
	fmt.Printf("║    Coinbase reseal:          %12.2fms  ║\n", coinbaseMs)
	fmt.Printf("║    Top tree build:           %12.2fms  ║\n", topTreeMs)
	fmt.Printf("║    BUMP assembly (%4dtok):  %12.2fms  ║\n", numBusinesses, bumpAssemblyMs)
	fmt.Printf("║    Disk reads:               %13d  ║\n", diskReads)
	fmt.Printf("║    Avg disk read:            %12.2fms  ║\n", avgDiskReadMs)
	fmt.Printf("║    Cache hits/misses:      %6d / %6d  ║\n", cacheHits, cacheMisses)
	fmt.Printf("║    BUMPs assembled:          %13d  ║\n", bumpCount)
	fmt.Printf("║    Avg BUMP size:            %11d B  ║\n", avgBumpBytes)
	fmt.Printf("║    Total BUMP bytes:         %11d B  ║\n", bumpBytes)
	fmt.Printf("╚══════════════════════════════════════════════╝\n\n")
}

func avgMs(totalNs, count int64) float64 {
	if count == 0 {
		return 0
	}
	return float64(totalNs) / float64(count) / 1e6
}
