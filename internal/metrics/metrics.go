// Package metrics collects and reports timing data for the STUMPT harness.
package metrics

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Collector accumulates timing and count data during the test run.
type Collector struct {
	startTime time.Time

	// Submission
	submitted atomic.Int64

	// Subtree sealing — all miners combined
	subtreeSealCount   atomic.Int64
	subtreeSealTotalNs atomic.Int64

	// Proof pre-computation (miner-0 proofs computed at subtree seal time)
	proofComputeCount   atomic.Int64
	proofComputeTotalNs atomic.Int64

	// Block finalization phases
	topTreeNs    atomic.Int64
	bumpAssembly atomic.Int64

	// Callback delivery
	cbCount      atomic.Int64
	cbTotalNs    atomic.Int64
	cbTotalBytes atomic.Int64

	mu        sync.Mutex
	bumpSizes []int
}

// NewCollector creates a new Collector and records the start time.
func NewCollector() *Collector {
	return &Collector{startTime: time.Now()}
}

// RecordSubmit notes one txid sent to the merkle service.
func (c *Collector) RecordSubmit() { c.submitted.Add(1) }

// RecordSubtreeSeal records how long it took to build all miners' subtrees.
func (c *Collector) RecordSubtreeSeal(d time.Duration) {
	c.subtreeSealCount.Add(1)
	c.subtreeSealTotalNs.Add(d.Nanoseconds())
}

// RecordProofCompute records how long pre-computing miner-0 proofs took for one subtree.
func (c *Collector) RecordProofCompute(d time.Duration) {
	c.proofComputeCount.Add(1)
	c.proofComputeTotalNs.Add(d.Nanoseconds())
}

// RecordTopTreeBuild records the top-tree assembly duration at block time.
func (c *Collector) RecordTopTreeBuild(d time.Duration) { c.topTreeNs.Store(d.Nanoseconds()) }

// RecordBUMPAssembly records the total BUMP compound-assembly duration.
func (c *Collector) RecordBUMPAssembly(d time.Duration) { c.bumpAssembly.Store(d.Nanoseconds()) }

// RecordCallback records a single BUMP delivery to the callback server.
func (c *Collector) RecordCallback(d time.Duration, bytes int) {
	c.cbCount.Add(1)
	c.cbTotalNs.Add(d.Nanoseconds())
	c.cbTotalBytes.Add(int64(bytes))
	c.mu.Lock()
	c.bumpSizes = append(c.bumpSizes, bytes)
	c.mu.Unlock()
}

// PrintSummary writes the final human-readable report to stdout.
func (c *Collector) PrintSummary() {
	elapsed := time.Since(c.startTime)
	submitted := c.submitted.Load()
	actualRate := float64(submitted) / elapsed.Seconds()

	sealCount := c.subtreeSealCount.Load()
	proofCount := c.proofComputeCount.Load()
	cbCount := c.cbCount.Load()
	cbBytes := c.cbTotalBytes.Load()

	avgSealMs := avgMs(c.subtreeSealTotalNs.Load(), sealCount)
	avgProofMs := avgMs(c.proofComputeTotalNs.Load(), proofCount)
	topTreeMs := float64(c.topTreeNs.Load()) / 1e6
	bumpAssemblyMs := float64(c.bumpAssembly.Load()) / 1e6
	avgCbMs := avgMs(c.cbTotalNs.Load(), cbCount)

	var avgBumpBytes int64
	if cbCount > 0 {
		avgBumpBytes = cbBytes / cbCount
	}

	slog.Info("test complete",
		"elapsed", elapsed.Round(time.Millisecond),
		"submitted", submitted,
		"rate_per_sec", fmt.Sprintf("%.1f", actualRate),
	)

	fmt.Printf("\n╔══════════════════════════════════════════╗\n")
	fmt.Printf("║          STUMPT FINAL SUMMARY            ║\n")
	fmt.Printf("╠══════════════════════════════════════════╣\n")
	fmt.Printf("║  Elapsed:                %15s  ║\n", elapsed.Round(time.Millisecond))
	fmt.Printf("║  Txids submitted:        %15d  ║\n", submitted)
	fmt.Printf("║  Actual rate:            %12.1f/s  ║\n", actualRate)
	fmt.Printf("╠══════════════════════════════════════════╣\n")
	fmt.Printf("║  Subtrees sealed:        %15d  ║\n", sealCount)
	fmt.Printf("║  Avg seal time:          %12.2fms  ║\n", avgSealMs)
	fmt.Printf("║  Proof pre-computations: %15d  ║\n", proofCount)
	fmt.Printf("║  Avg proof time:         %12.2fms  ║\n", avgProofMs)
	fmt.Printf("╠══════════════════════════════════════════╣\n")
	fmt.Printf("║  Top tree build:         %12.2fms  ║\n", topTreeMs)
	fmt.Printf("║  BUMP assembly (100tok): %12.2fms  ║\n", bumpAssemblyMs)
	fmt.Printf("║  Callbacks delivered:    %15d  ║\n", cbCount)
	fmt.Printf("║  Avg callback time:      %12.2fms  ║\n", avgCbMs)
	fmt.Printf("║  Avg BUMP size:          %12d B  ║\n", avgBumpBytes)
	fmt.Printf("║  Total BUMP bytes:       %12d B  ║\n", cbBytes)
	fmt.Printf("╚══════════════════════════════════════════╝\n\n")
}

func avgMs(totalNs, count int64) float64 {
	if count == 0 {
		return 0
	}
	return float64(totalNs) / float64(count) / 1e6
}
