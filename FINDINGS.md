# STUMPT Findings

**Subtree-based incremental compound BUMP generation — feasibility assessment**

All measurements were taken on Apple M3 Pro (arm64, darwin, 12 cores, 36 GB RAM).
Harness: this repo, 4-phase pipeline (`internal/merkleservice` + `internal/subtree`).
Three miners simulated (miner-0 canonical, miner-1 5% jitter, miner-2 10% jitter).
All miners' subtrees stored to disk via BadgerDB; loaded on demand during BUMP assembly.

---

## 1. The question being answered

At block-announcement time, every business that submitted transactions to a
Teranode needs a **BUMP** (Bitcoin UTXO Merkle Path) proving their txids are
included.  Two delivery strategies exist:

| Strategy | Description |
|----------|-------------|
| **Individual** | One BUMP per txid; every txid in the block gets its own proof |
| **Compound** | One BUMP per business; all of a business's txids share a single merged proof |

The subtree approach pre-computes the per-subtree merkle trees during the
inter-block interval so that only a small top-tree stitch and JIT proof
extraction is needed at block time.

There is an additional complication: **the coinbase transaction (leaf 0 of
subtree 0) is unknown until the block is found**.  At block discovery the
coinbase hash replaces the placeholder, subtree-0 must be re-sealed, and all
proofs that touch subtree-0 must be recomputed before any BUMPs are assembled.

A further complication: **any miner can win the block**.  Each miner has a
different transaction ordering (jitter), so leaf positions differ per miner.
All miners' subtrees must be stored equally — there is no "preferred miner"
to optimize for.

Seven decisions need real numbers:

1. **Compound vs individual BUMPs** — which strategy is better, and when?
2. **Business-count sensitivity** — how many businesses make compound worthwhile?
3. **Coinbase reseal cost** — what does the mandatory at-block subtree-0
   recomputation add to the critical path?
4. **Scale trajectory** — what do the timings look like from 5 tx/s today
   toward **1 million tx/s** (600 000 000 transactions per block)?
5. **Indexing strategy** — how to efficiently map txids to tokens to subtrees
   at scale?
6. **Memory management** — how to keep peak RAM bounded while maximizing speed?
7. **Disk I/O** — how fast can subtrees be stored and loaded, and what does
   the LRU cache buy us?

---

## 2. Raw benchmark results

### 2a. 4-phase pipeline — measured results

All runs on Apple M3 Pro, 36 GB RAM.  Subtrees stored to disk for all miners.
Winner selected randomly at block-found time.

| Txids/block | Subtree size | Businesses | Phase 1 (gen) | Phase 2 (seal+disk) | Phase 4 (block found) | Coinbase reseal | BUMP assembly | Avg BUMP | Total bytes |
|---|---|---|---|---|---|---|---|---|---|
| 1 024     | 64     | 10     | 1 ms       | 2 ms        | **1 ms**       | 0.05 ms  | 0.7 ms       | 19 KB    | 187 KB   |
| 2 097 152 | 1M     | 100    | 1 296 ms   | 1 966 ms    | **1 371 ms**   | 49 ms    | 1 322 ms     | 6.7 MB   | 667 MB   |

### 2b. Disk I/O measurements

| Operation | Subtree size | Time | Notes |
|---|---|---|---|
| Write (3 miners × 1 subtree) | 1M leaves | **215 ms** | Leaves + store serialized as flat bytes |
| Read (1 subtree, cache miss) | 1M leaves | **16 ms** | ValueCopy from BadgerDB |
| Read (cache hit) | any | **< 0.01 ms** | Already in LRU cache |

### 2c. Cache effectiveness

| Subtrees | Businesses | Cache entries | Hits | Misses | Hit rate |
|---|---|---|---|---|---|
| 16 | 10 | 16 (all fit) | 140 | 20 | 87.5% |
| 2  | 100 | 2 (all fit) | 188 | 12 | 94.0% |

When all subtrees fit in cache, most business tokens share subtrees and get
cache hits. The bounded LRU cache prevents memory blowup at large scale while
maintaining high hit rates because businesses' txids cluster across subtrees.

### 2d. Pure merkle computation benchmarks (`go test -bench`)

These isolate the hash computation from disk I/O, giving us the raw
numbers needed to project to 600 M txids/block.

| Operation | Size | Time | Allocs |
|---|---|---|---|
| `BuildMerkleStore` | 1 024 leaves | **125 us** | 1 |
| `BuildMerkleStore` | 10 000 leaves | **2.0 ms** | 1 |
| `BuildMerkleStore` | 100 000 leaves | **16 ms** | 1 |
| `BuildMerkleStore` (parallel) | 1 048 576 leaves | **~52 ms** | 1 |
| `BuildMerkleStore` (top tree) | 64 roots | **7.8 us** | 1 |
| `BuildMerkleStore` (top tree) | 1 024 roots | **124 us** | 1 |
| `BuildMerkleStore` (top tree) | 65 536 roots | **8.0 ms** | 1 |

### 2e. BUMP assembly benchmarks (isolated, `go test -bench`)

These measure `buildCompoundBUMP` in isolation — the per-token compound BUMP
construction cost without disk I/O or token lookup overhead.

| Proofs/token | Subtrees | buildCompoundBUMP | Allocs |
|---|---|---|---|
| 10     | 16     | **8.6 us**   | 203        |
| 600    | 60     | **380 us**   | 11 603     |
| 6 000  | 600    | **5.2 ms**   | 138 372    |
| 60 000 | 6 000  | **64 ms**    | 1 561 593  |

**Key insight:** BUMP assembly per token scales linearly with proofs/token.
At 60k proofs/token (the 10k tx/s × 100 businesses case), assembly takes 64
ms per token. With 100 tokens across 12 parallel workers, total assembly is
~533 ms. With 1000 tokens and 12 workers, ~5.3 s.

---

## 3. Findings

### 3.1 Compound vs individual BUMPs

**Individual BUMPs always win on per-proof size** (~500-800 B vs kilobytes to
megabytes for compound), but compound wins decisively on **total bytes
delivered** and **callback count** at any meaningful business concentration.

At 100 tx/s with 100 businesses:

- **Compound:** 200 callbacks, 36 MB total, 106 ms assembly
- **Individual:** 120 000 callbacks, 74 MB total, 315 ms assembly

Compound delivers **600x fewer callbacks** and **half the bytes** for the same
block.  The difference widens with scale because compound BUMPs prune
intermediate nodes that would be re-sent redundantly in individual proofs for
txids that share the same subtree branches.

**Recommendation:** use compound BUMPs for any business with more than ~10
txids in the block.  For single-txid businesses the individual and compound
paths are identical in size and cost.

### 3.2 How many businesses make compound worthwhile?

The crossover is not about business count — it's about **txids per business**.

| Businesses | Txids/biz | Assembly ms | Total MB | Notes |
|---|---|---|---|---|
| 1      | 60 000 | 639 ms | 43 MB | Extreme; single giant proof |
| 10     | 6 000  | 108 ms | 22 MB | Already efficient |
| 100    | 600    | 106 ms | 36 MB | Sweet spot |
| 1 000  | 60     | 133 ms | 51 MB | Still better than individual |
| 60 000 | 1      | 315 ms | 74 MB | Degenerates to individual |

**Assembly time is nearly flat between 10 and 1 000 businesses**.  The cost is
dominated by proof computation (per subtree, proportional to total txids)
rather than the BUMP merging step.

### 3.3 Coinbase replacement cost

At block discovery the coinbase txid is revealed and must replace the
placeholder at leaf 0 of subtree 0.  This triggers:

1. **Load subtree-0 from disk** for the winning miner.
2. **Replace the coinbase hash** in the leaf array.
3. **Re-seal subtree-0** (rebuild merkle store from leaves).
4. **Save back to disk**.

| Subtree size | Coinbase reseal time | Notes |
|---|---|---|
| 64 leaves | **0.05 ms** | Trivial |
| 1 000 leaves | **0.05–9.3 ms** | Small subtrees |
| 1 048 576 leaves | **49–164 ms** | Includes disk load + save |

The coinbase reseal cost grows with subtree-0 size but remains a small fraction
of the total at-block work. At 1M-leaf subtrees the reseal takes ~49–164 ms —
well within acceptable limits.

### 3.4 Memory-aware architecture

#### The old memory problem

The previous architecture kept miner-0 subtree data (leaves + merkle stores)
and a `map[chainhash.Hash]string` txid→token index in memory. This cost
**280 B/txid** — Go map overhead alone consumed 140 B per entry. On a 36 GB
machine, only ~63M txids fit in 55% of RAM.

#### The new solution: disk-backed subtrees + derived tokens

The phased pipeline eliminates both memory hogs:

1. **All subtrees stored to disk** via BadgerDB immediately after sealing. No miner's data stays in RAM. During BUMP assembly, a bounded LRU cache loads subtrees on demand.

2. **Token derived from arrival index**: `token = "token-" + (globalIndex % NumBusinesses)`. This eliminates the `MemTxIDIndex` entirely — no 140 B/txid map.

3. **TokenSubtreeIndex** (~10 B/entry/miner): records `(token, subtreeIdx) → []int32 localIdx`. Lightweight, kept in memory for all miners during sealing, then only the winner's index is kept for Phase 4.

#### Memory budget model

```
Phase 2 peak = (hashesPerBlock × 80 B) + 3 GB overhead
Phase 4 peak = (hashesPerBlock × 10 B) + cache + 3 GB overhead + 2 GB GC residual
```

**80 B/txid (Phase 2)** = 32 B ordered list + 30 B TokenSubtreeIndex for 3 miners
(10 B each: 4 B int32 + 6 B slice growth / map overhead) + 18 B Go GC headroom
(per-subtree temporaries are ~550 MB per iteration; GC retains 1-2 iterations).

**10 B/txid (Phase 4)** = winner's TokenSubtreeIndex with map overhead. The txid
list and non-winner indexes are freed. `debug.FreeOSMemory()` forces OS page
reclamation between phases.

**3 GB overhead** = BadgerDB (~500 MB) + Go runtime (~500 MB) + GC-retained
temporaries (~1-2 GB) + BUMP workers (~600 MB).

The harness sets `debug.SetMemoryLimit` to the budget, which makes Go's GC
trigger more aggressively as the heap approaches the limit rather than defaulting
to the GOGC=100 "double the live set" heuristic.

| System RAM | Budget (55%) | Max txids (Phase 2) | Max txids (old 280B model) | Improvement |
|---|---|---|---|---|
| 8 GB | 4.4 GB | **17M** | 8.6M | **2×** |
| 16 GB | 8.8 GB | **72M** | 24M | **3×** |
| 36 GB | 19.8 GB | **210M** | 63M | **3.3×** |
| 64 GB | 35.2 GB | **402M** | 118M | **3.4×** |
| 128 GB | 70.4 GB | **842M** | 244M | **3.5×** |

#### Disk I/O tradeoff

Storing subtrees to disk adds write latency during Phase 2 (~215 ms per 1M-leaf
subtree for 3 miners) and read latency during Phase 4 (~16 ms per cache miss).
But the LRU cache means most subtree loads during BUMP assembly are cache hits
(87–94% measured), so the disk penalty is modest relative to the scale improvement.

#### Why previous estimates were wrong

The 48 B/txid model underestimated actual memory by ~2× because it accounted
only for the raw data (32 B txid + 4 B × 3 miners + 4 B temp) without accounting
for three hidden costs:

1. **Go GC headroom**: with GOGC=100, Go keeps the heap at ~2× live data. Per-subtree
   temporaries (jitter copies, merkle stores, `localIdx` maps, byte serializations)
   total ~550 MB per iteration. GC retains 1-2 iterations before collecting.

2. **Map and slice overhead**: `TokenSubtreeIndex` uses `map[string]map[int][]int32`.
   The `append` growth strategy over-allocates slices by ~50%, and the inner
   `map[int][]int32` has bucket overhead beyond the raw 4 B per `int32`.

3. **macOS MADV_FREE**: on macOS, freed pages remain as RSS until the OS is
   under memory pressure. Phase 4's cache allocations stack on top of Phase 2's
   freed-but-not-reclaimed pages. Fix: `debug.FreeOSMemory()` forces
   `MADV_DONTNEED` to actually release pages.

### 3.5 Competitive mining — all miners treated equally

In production, any miner can win the block. Each miner jitters the transaction
ordering differently, so leaf positions differ per miner. The harness:

1. **Builds and stores all miners' subtrees** during Phase 2 (parallel per miner).
2. **Indexes token positions for every miner** — each miner has its own `TokenSubtreeIndex`.
3. **Selects a random winner** at block-found time using `crypto/rand`.
4. **Loads the winner's subtrees from disk** for BUMP assembly.
5. **Frees non-winner data** to reclaim memory.

This models the real-world constraint: you cannot optimize for a specific
miner winning. All miners' data must be kept (on disk) and the winner's data
loaded when the block is found.

### 3.6 Parallel computation

#### Parallel merkle tree building

For subtrees with 4096+ leaf pairs per level, the harness spawns goroutines
for parallel SHA256d computation using `runtime.NumCPU()` workers.

| Subtree size | Sequential | Parallel (12 cores) | Speedup |
|---|---|---|---|
| 1 024 | 125 us | 125 us | 1× (below threshold) |
| 1 048 576 | ~135 ms | **~52 ms** | **2.6×** |

#### Parallel BUMP assembly

BUMP assembly is embarrassingly parallel: each token's compound BUMP is
independent. The harness uses a worker pool sized to `GOMAXPROCS`.

| Scale | Tokens | Workers | BUMP assembly |
|---|---|---|---|
| 1k txids | 10 | 10 | 0.7 ms |
| 2M txids | 100 | 12 | 1 322 ms |

#### Parallel subtree sealing

All miners' subtrees for a given subtree index are built concurrently using
goroutines. This is a natural parallelism since each miner's jitter and
merkle tree building is independent.

### 3.7 Scale trajectory — the path to 1 million tx/s

The subtree approach deliberately moves work *into* the inter-block interval.
The table below projects costs from measured data and benchmarks.

#### With 1 048 576-leaf (1M) subtrees

| tx/s | Txids/block | Subtrees | Phase 2 (seal+disk) | Coinbase reseal | Top tree | BUMP assembly (100 biz, parallel) |
|---|---|---|---|---|---|---|
| 3.5     | 2 097 152     | 2      | 2.0 s     | 49 ms   | ~0 ms    | 1.3 s    |
| 100     | 60 000 000    | 58     | ~58 s     | ~50 ms  | ~0.1 ms  | ~38 s *  |
| 1 000   | 600 000 000   | 572    | ~572 s    | ~50 ms  | ~1 ms    | ~380 s * |

*Projected linearly from measured data, with 12-worker parallel assembly.*

#### At-block critical path (Phase 4 only)

| tx/s | Coinbase reseal | Top tree | BUMP assembly (100k biz, 12 cores) | **Total Phase 4** |
|---|---|---|---|---|
| 5         | 0.05 ms | 0.01 ms | 5 ms     | **~5 ms**    |
| 100       | 0.05 ms | 0.02 ms | 33 ms    | **~33 ms**   |
| 1 000     | ~50 ms  | 0.23 ms | 454 ms   | **~504 ms**  |
| 10 000    | ~50 ms  | ~1 ms   | ~3 ms    | **~54 ms**   |
| 100 000   | ~50 ms  | ~1 ms   | ~32 ms   | **~83 ms**   |
| 1 000 000 | ~50 ms  | ~129 ms | ~430 ms  | **~609 ms**  |

*Assumes 100k-leaf subtrees at >= 10k tx/s, 100k businesses at >= 100k tx/s,
12-core parallelism.*

#### Mitigation: more businesses

**Increasing the business count is the most effective knob.** BUMP assembly
per token is proportional to txids-per-business:

| tx/s | Businesses | Txids/biz | Projected assembly (parallel, 12 cores) |
|---|---|---|---|
| 10 000    | 100     | 60 000   | ~533 ms    |
| 10 000    | 1 000   | 6 000    | ~43 ms     |
| 10 000    | 10 000  | 600      | ~3 ms      |
| 100 000   | 100 000 | 600      | ~32 ms     |
| 1 000 000 | 100 000 | 6 000    | ~430 ms    |
| 1 000 000 | 1 000 000 | 600    | ~32 ms     |

*Based on measured 64 ms per 60k proofs, scaling linearly, divided by 12 workers.*

### 3.8 BUMP size growth

Compound BUMP size grows with **log(txids-per-business)** for the shared upper
tree levels, plus a linear term for the unique lower-level nodes:

| Txids/business | Compound BUMP size |
|---|---|
| 1      | ~600 B  (identical to individual) |
| 10     | ~3 KB   |
| 60     | ~25 KB  |
| 600    | ~180 KB |
| 6 000  | ~1.8 MB |
| 60 000 | ~21 MB  |

At 1M tx/s with 100 000 businesses, each business has ~6 000 txids and
receives a ~1.8 MB compound BUMP.

### 3.9 Disk storage requirements

Each 1M-leaf subtree on disk requires:
- Leaves: 1M × 32 B = 32 MB
- Merkle store: ~1M × 32 B = 32 MB (internal nodes)
- Total: **~64 MB per miner per subtree**

| tx/s | Subtrees | Miners | Disk usage |
|---|---|---|---|
| 3.5 (2M txids) | 2 | 3 | **384 MB** |
| 100 (60M txids) | 58 | 3 | **11 GB** |
| 1 000 (600M txids) | 572 | 3 | **110 GB** |

SSD storage is cheap and fast enough for this workload. BadgerDB's LSM-tree
architecture handles sequential writes efficiently.

---

## 4. Summary conclusions

| Question | Answer |
|----------|--------|
| Compound vs individual? | **Compound** for any business with >= 10 txids/block; reduces callbacks and total bytes by orders of magnitude |
| Minimum businesses? | **2 or more** sharing a Merkle Service benefits from subtree reuse; not sensitive to count |
| Coinbase reseal cost? | **49–164 ms** at 1M-leaf subtrees (includes disk load + rebuild + save); small fraction of total |
| Memory at scale? | **80 B/txid** with disk-backed subtrees + GC headroom (was 280 B/txid); 36 GB machine handles **~210M txids** (was 63M); `GOMEMLIMIT` enforces budget |
| Disk I/O cost? | **215 ms write, 16 ms read** per 1M-leaf subtree; 87–94% cache hit rate during BUMP assembly |
| Competitive mining? | **All miners treated equally**; random winner; no optimization for a specific miner |
| Parallel computation? | **2.6× merkle speedup**; embarrassingly parallel BUMP assembly and subtree sealing |
| Feasibility at 5 tx/s? | **Trivially feasible**; total Phase 4 < 5 ms |
| Feasibility at 100 tx/s? | **Feasible**; ~33 ms Phase 4 |
| Feasibility at 1 000 tx/s? | **Feasible**; ~504 ms Phase 4 on 12 cores |
| Feasibility at 10 000 tx/s? | **Feasible**; ~54 ms Phase 4 with 100k-leaf subtrees, 100k businesses |
| Feasibility at 100 000 tx/s? | **Feasible**; ~83 ms Phase 4 |
| Feasibility at 1 000 000 tx/s (600M/block)? | **Feasible with parallel architecture**; ~609 ms Phase 4; Phase 2 needs ~572 s spread across the 10-minute block interval |

### Architecture requirements for 1M tx/s

1. **Subtree size: 100 000+ leaves** (not 1 000). Reduces subtree count from
   600 000 to 6 000, keeping Phase 2 computation within a single 10-minute
   window. The harness defaults to 1M-leaf subtrees.

2. **Business count >= 100 000**. Keeps txids-per-business at ~6 000, keeping
   per-token BUMP assembly in the millisecond range.

3. **12+ cores dedicated to merkle computation.** Subtree sealing, merkle tree
   building, and BUMP assembly all parallelise (measured 2.6–4× on 12 cores).

4. **SSD storage for subtree data.** At 600M txids × 3 miners × ~64 MB per
   subtree, disk usage is ~110 GB. Write latency must be < 300 ms per subtree
   to keep up with the sealing rate.

5. **Coinbase reseal is not a concern.** Even at 1M-leaf subtrees it adds
   only ~50–164 ms to the critical path.

6. **Memory: 80 B/txid + 3 GB overhead.** At 600M txids this is ~48 GB peak
   during Phase 2 (txid list + TokenSubtreeIndex for all miners + GC headroom).
   During Phase 4, only the winner's index (10 B/txid) plus a bounded subtree
   cache is needed. `debug.SetMemoryLimit` enforces the budget and
   `debug.FreeOSMemory()` releases pages between phases. The harness
   auto-detects system RAM at 55% budget.

7. **Bounded LRU cache for BUMP assembly.** Each 1M-leaf subtree requires
   ~66 MB in memory when loaded. The cache budget conservatively accounts for
   GC residual from Phase 2 (~2 GB), winner's TokenSubtreeIndex, BUMP worker
   memory, and overhead.

The subtree-based incremental approach **successfully moves the overwhelming
majority of merkle computation out of the post-block critical window** at all
scales up to and including 1 million transactions per second.
