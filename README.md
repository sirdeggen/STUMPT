# STUMPT

**S**ubTree Unified **M**erkle **P**ath **T**esting

A performance test harness for the theory behind incremental compound BUMP (BRC-74) generation at block-scale transaction volumes.

---

## What is this testing?

Bitcoin SV blocks at Teranode scale contain millions of transactions.  When a block is found, every business that submitted transactions needs to receive a **BUMP** (Bitcoin UTXO Merkle Path) — a compact proof that their transactions are included in the block.

The naive approach is to build the full block merkle tree at the moment the block is found and then compute proofs for all subscribers.  At 600 million transactions per block that is not feasible in the time available.

The theory being tested here is a **subtree-based incremental approach**:

1. As transactions arrive over the ~10 minutes between blocks, group them into fixed-size **subtrees** (default: 1M leaves each).
2. Each subtree's internal merkle tree is built as soon as the subtree is sealed — spreading the work evenly across the inter-block interval.
3. All miners build their own subtrees (with jittered orderings) and **store them to disk** via BadgerDB.
4. A lightweight **TokenSubtreeIndex** records which business tokens have txids in each subtree and their leaf positions (4 bytes/entry).
5. **STUMP fragments** (per-token, per-subtree BUMP proof data) are pre-computed during sealing while the merkle store is in memory, and stored to disk. This moves the expensive proof extraction into Phase 2 instead of the critical path.
6. When the block is found, a random miner wins. The coinbase is revealed, subtree-0 is resealed (and its fragments re-extracted), a small **top tree** is built, and **pre-computed fragments are loaded** (~82 KB each instead of 64 MB full subtrees) to assemble final BUMPs.
7. Each subscribing business receives a single **compound BUMP** that covers all of their transactions at once.

This repo simulates that pipeline end-to-end across 4 phases and measures the timing of each.

See also:
- **[ARCHITECTURE.md](ARCHITECTURE.md)** — Storage formats, key layouts, data flow diagrams, fragment encoding, and size calculations
- **[FINDINGS.md](FINDINGS.md)** — Benchmark results, scale projections, feasibility analysis from 5 tx/s to 1M tx/s

---

## System requirements

The harness auto-detects your system's RAM and scales the default test to use ~55% of physical memory.

```
╔══════════════════════════════════════════════════════════════════════════╗
║                     STUMPT System Requirements                          ║
╠═══════════════╦═══════════════╦═══════════════╦══════════════════════════╣
║   Total TXIDs ║   Subtrees    ║   Est. RAM    ║   Min System RAM        ║
╠═══════════════╬═══════════════╬═══════════════╬══════════════════════════╣
║            2M ║       2 × 1M ║        3.2 GB ║        5.7 GB             ║
║            4M ║       4 × 1M ║        3.3 GB ║        6.0 GB             ║
║           10M ║      10 × 1M ║        3.8 GB ║        6.9 GB             ║
║           21M ║      20 × 1M ║        4.6 GB ║        8.3 GB             ║
║           42M ║      40 × 1M ║        6.1 GB ║       11.1 GB             ║
║           63M ║      60 × 1M ║        7.7 GB ║       14.0 GB             ║
║          105M ║     100 × 1M ║       10.8 GB ║       19.7 GB             ║
║          157M ║     150 × 1M ║       14.7 GB ║       26.8 GB             ║
║          315M ║     300 × 1M ║       26.4 GB ║       48.1 GB             ║
║          629M ║     600 × 1M ║       49.9 GB ║       90.7 GB             ║
╚═══════════════╩═══════════════╩═══════════════╩══════════════════════════╝

Memory model: 80 B/txid (50B base + 3×10B per miner) + 3 GB overhead
Includes Go GC headroom, map/slice overhead, and per-subtree temporary allocations.
Subtrees stored to disk via BadgerDB — not counted in per-txid memory.
```

**80 B/txid** (for 3 miners) breaks down as:
- 32 B — in-memory txid list (ordered `[]chainhash.Hash` slice, freed after Phase 2)
- 30 B — TokenSubtreeIndex for 3 miners (10 B each: 4 B `int32` data + 6 B slice growth / map bucket overhead)
- 18 B — Go GC headroom (per-subtree temporaries: jitter copies, merkle stores, `localIdx` maps, byte serializations are ~550 MB per iteration; GC retains 1-2 iterations of garbage)

**3 GB overhead** covers BadgerDB memtables/caches (~500 MB), Go runtime/GC base (~500 MB), GC-retained temporary allocations (~1-2 GB), and BUMP assembly workers (~600 MB for 12 workers × 50 MB).

The harness sets `debug.SetMemoryLimit` to the budget so Go's GC triggers more aggressively as the heap approaches the limit, and calls `debug.FreeOSMemory()` between phases to force the OS to reclaim freed pages.

Subtree data (leaves + merkle stores) is stored to disk for all miners — not kept in RAM. During BUMP assembly, a bounded LRU cache loads subtrees on demand.

The default budget is **55% of system RAM** — conservative enough to avoid swap even under GC pressure. If your machine doesn't have enough RAM for the requested configuration, the harness exits with a clear error and prints this table. Use `-max-memory` to set a specific budget, or `-requirements` to just print the table.

---

## Architecture

The harness runs a 4-phase pipeline in a single process:

```
Phase 1: Generate txids
    ↓
Phase 2: Seal subtrees to disk (all miners) + index token positions
    ↓
Phase 3: (done during Phase 2) TokenSubtreeIndex built
    ↓
Phase 4: Block found → load from disk → assemble BUMPs (critical path)
```

### Phase 1 — Txid Generation

Generate `HashesPerBlock` random 32-byte hashes into a `[]chainhash.Hash` slice. Each txid is associated with a business token derived from the txid hash itself: `token-{uint64(txid[:8]) % NumBusinesses}`. This gives uniform, order-independent random assignment. Parallel generation across all CPU cores.

**Metric:** generation rate (txids/sec), total time.

### Phase 2 — Miner Subtree Sealing + STUMP Fragment Pre-computation

For each subtree boundary (every `HashesPerSubtree` txids):
- Slice base txids from the Phase 1 list
- For each miner (in parallel): jitter txids, `BuildMerkleStore()`, save leaves+store to BadgerDB
- Build per-miner `TokenSubtreeIndex` entries (derive token from txid hash)
- **Extract STUMP fragments:** for each token's txids in this subtree, compute the subtree-level BUMP entries (levels 0..subtreeHeight-1) using bitset dedup, and store the serialized fragment to disk (~82 KB per token per subtree)
- Free in-memory subtree data immediately after saving to disk
- Record subtree roots per miner

After all subtrees are sealed, the Phase 1 txid list is freed.

**Key insight:** Token is derived from the txid hash — deterministic, order-independent, and uniform. STUMP fragments are extracted while the merkle store is in memory, avoiding the need to reload 64 MB subtrees during Phase 4.

**Metric:** seal+write rate (subtrees/sec), avg seal time, avg disk write time.

### Phase 3 — Token Index (integrated into Phase 2)

The `TokenSubtreeIndex` is built during Phase 2 as each subtree is sealed. It records `(token, subtreeIdx) → []int32 localIdx` for every miner. At 4B per entry per miner, this is the lightweight index that Phase 4 uses to know which fragments to load.

### Phase 4 — Block Found (critical path, timed)

Timer starts. A random miner wins.

1. **Coinbase reseal:** Load winner's subtree-0 from disk, replace coinbase placeholder, rebuild store, save back. Re-extract STUMP fragments for all tokens in subtree-0.
2. **Top-tree build:** Build merkle tree from winner's subtree roots.
3. **BUMP assembly:** For each business token:
   - Look up subtree indices from winner's `TokenSubtreeIndex`
   - Load pre-computed STUMP fragments from disk (~82 KB each, vs 64 MB full subtrees)
   - Deserialize fragment entries, add top-tree proof levels, prune, serialize BRC-74 binary
   - Record BUMP size and assembly time

Non-winner `TokenSubtreeIndex` data is freed at the start of this phase.

**Metric:** total critical-path time, coinbase reseal, top tree build, BUMP assembly time, per-token BUMP size, fragment load rate.

### Storage strategy

| Data | Storage | Why |
|------|---------|-----|
| Ordered txid list | **In-memory** (Phase 1-2) | Pre-allocated to block size; freed after sealing |
| All miners' subtree leaves + stores | **BadgerDB (disk)** | Stored immediately after sealing; used as fallback if fragments unavailable |
| STUMP fragments (per token × subtree) | **BadgerDB (disk)** | Pre-computed during Phase 2; ~82 KB each (vs 64 MB full subtree) |
| TokenSubtreeIndex (all miners) | **In-memory** | Lightweight (4B/entry/miner); maps tokens to subtrees for fragment lookup |

### Subtree / Merkle Engine (`internal/subtree`)
- Implements `BuildMerkleStore` and `GetMerkleProof` using `go-sdk/chainhash` only.
- Parallel merkle tree building: spawns goroutines for levels with 4096+ hash pairs.
- `HashPair` is byte-identical to `transaction.MerkleTreeParent` in go-sdk, verified by test.

### TokenSubtreeIndex (`internal/merkleservice`)
- Lightweight replacement for the full STUMP store.
- Maps `(token, subtreeIdx) → []int32` (local leaf positions within the subtree).
- 4 bytes per indexed txid vs ~860 bytes for pre-computed full proofs.
- Thread-safe with `sync.RWMutex`.
- BUMP assembly uses these positions to compute proofs JIT from loaded merkle stores.

### Disk Store (`internal/diskstore`)
- BadgerDB v4 LSM-tree for persistent key-value storage.
- `MinerSubtreeStore`: saves/loads subtree leaves and merkle stores for all miners.
  Key format: `'m' + minerIdx(4B) + subtreeIdx(4B) + type('L'|'S')`.
- `FragmentStore`: saves/loads pre-computed STUMP fragments per (miner, token, subtree).
  Key format: `'f' + minerIdx(4B) + tokenIdx(4B) + subtreeIdx(4B)`.
  Each fragment is ~82 KB containing deduped BUMP entries for subtree levels.

---

## Default configuration

The defaults are auto-detected based on your system's available RAM:

| Parameter | Default | Notes |
|-----------|---------|-------|
| `HASHES_PER_SUBTREE` | 1 048 576 (1M) | subtree height = 20 |
| `HASHES_PER_BLOCK` | auto-detected | fills ~55% of system RAM at 48 B/txid |
| Num miners | 3 | competing subtree orderings |
| Num businesses | 1 000 | distinct callback tokens |
| Mock block height | 800 000 | stamped in every BUMP |

For example, on a 36 GB machine the default is ~208 subtrees × 1M = **~218M txids** (~19.7 GB estimated).
On a 16 GB machine: ~68 subtrees × 1M = **~71M txids**.

---

## Prerequisites

- Go 1.24 or later (`go version`)

---

## Running the test

### 1. Clone and build

```bash
git clone https://github.com/bsv-blockchain/stumpt
cd stumpt
go build -o harness ./cmd/harness/
```

### 2. Run with auto-detected defaults

```bash
./harness
```

The harness auto-detects your system's RAM and runs the largest test that fits in ~55% of physical memory with 1M-leaf subtrees. Structured JSON logs stream to stdout. When all BUMPs are assembled, a summary table is printed:

```
╔══════════════════════════════════════════════╗
║            STUMPT FINAL SUMMARY              ║
╠══════════════════════════════════════════════╣
║  Total elapsed:                     7.672s  ║
║  Total txids:                      2097152  ║
╠══════════════════════════════════════════════╣
║  PHASE 1 — Generation                1442ms  ║
║    Rate:                        1454291/s  ║
╠══════════════════════════════════════════════╣
║  PHASE 2 — Seal + Index              5254ms  ║
║    Subtrees sealed:                      2  ║
║    Avg seal time:                   53.52ms  ║
║    Disk writes:                          2  ║
║    Avg disk write:                  86.16ms  ║
╠══════════════════════════════════════════════╣
║  PHASE 4 — Block Found                934ms  ║
║    Coinbase reseal:                729.81ms  ║
║    Top tree build:                   0.00ms  ║
║    BUMP assembly ( 100tok):        196.01ms  ║
║    Disk reads:                         200  ║
║    Avg disk read:                    1.11ms  ║
║    Cache hits/misses:         200 /      0  ║
║    BUMPs assembled:                    100  ║
║    Avg BUMP size:                6250467 B  ║
║    Total BUMP bytes:           625046778 B  ║
╚══════════════════════════════════════════════╝
```

Phase 2 is longer because it now pre-computes STUMP fragments for every token in every subtree. Phase 4 BUMP assembly is dramatically faster: fragments are ~82 KB each (vs 64 MB full subtrees), giving 100% cache hits and eliminating the merkle store traversal entirely.

### 3. Constrained memory budget

```bash
# Limit to 10 GB peak — auto-scales to fit
./harness -max-memory 10
```

### 4. Print system requirements

```bash
./harness -requirements
```

Prints the requirements table and exits without running any test.

### 5. Explicit scale

```bash
# 2M txids: 2 subtrees × 1M leaves, 100 businesses
./harness -hashes-per-block 2097152 -hashes-per-subtree 1048576 -businesses 100

# 60M txids: 60 subtrees × 1M leaves, 1000 businesses (needs ~4 GB)
./harness -hashes-per-block 62914560 -hashes-per-subtree 1048576 -businesses 1000
```

If the requested configuration exceeds available memory, the harness exits with an error:
```
ERROR: estimated memory 5.7 GB exceeds budget 5.0 GB (105M txids × 48 B/txid + 1.0 GB overhead)
Reduce -hashes-per-block or -hashes-per-subtree, or increase -max-memory
```

### 6. All CLI flags

```
-hashes-per-block   int      Total txids per simulated block    (default: auto-detected from RAM)
-hashes-per-subtree int      Txids per subtree                  (default 1048576)
-miners             int      Number of competing miners         (default 3)
-businesses         int      Distinct callback tokens           (default 1000)
-dump-bump          string   Write first assembled BUMP as hex to this file (optional)
-data-dir           string   BadgerDB data directory            (empty = temp dir)
-max-memory         float    Peak memory budget in GB           (default: 55% of system RAM)
-requirements                Print system requirements table and exit
```

`hashes-per-block` must be divisible by `hashes-per-subtree`.

---

## Running the tests

The merkle engine and index have full unit + integration test suites.

```bash
go test ./...
```

With the race detector:

```bash
go test -race ./...
```

Benchmarks:

```bash
# Merkle engine benchmarks
go test -bench=. -benchmem ./internal/subtree/

# BUMP assembly benchmarks (isolated from disk I/O)
go test -bench=. -benchmem ./internal/merkleservice/
```

Key tests:

| Package | Test | What it checks |
|---------|------|----------------|
| `subtree` | `TestHashPairMatchesGoSDK` | `HashPair` is byte-identical to `transaction.MerkleTreeParent` |
| `subtree` | `TestProofRoundTripSmall` | Proofs for n = 1, 2, 3, 4, 7, 8 leaves all verify |
| `subtree` | `TestProofRoundTrip1024` | Every 64th leaf of a 1024-leaf subtree verifies |
| `subtree` | `TestCompoundBUMPCombine` | Two per-txid BUMPs combine and verify |
| `subtree` | `TestGetAllProofs` | Batch proof generation matches single-proof generation |
| `subtree` | `TestCompoundBUMPAcrossSubtrees` | 4 subtrees × 4 leaves: compound BUMPs verify against block root |
| `subtree` | `TestCompoundBUMPDefaultConfig` | 61,440-txid / 60-subtree / 16-level tree: sample compound BUMPs verify |
| `merkleservice` | `TestFastVsLegacyBUMP` | Bitset-based fast path produces byte-identical BUMPs to legacy map-based path |
| `merkleservice` | `TestFragmentVsJIT` | Fragment-based assembly produces byte-identical BUMPs to JIT assembly |

---

## What the numbers tell you

The key insight this harness measures is the **work distribution**:

- **Phase 1 (generation):** ~1.6M txids/sec parallel generation. This models the txid arrival rate.
- **Phase 2 (sealing):** subtree sealing + merkle tree building runs for each subtree boundary. Each 1M-leaf subtree seal takes ~50 ms. Disk writes add ~215 ms per subtree. All miners are sealed in parallel.
- **Phase 4 (block found — critical path):** only the coinbase reseal (~49 ms), top-tree build (microseconds), and JIT BUMP assembly need to happen. Subtrees are loaded from disk with a bounded LRU cache.
- **Per-business BUMP size** reflects how many txids that business submitted. Businesses with more txids get larger but more compact compound BUMPs (intermediate hashes are pruned).

---

## Dependencies

| Package | Use |
|---------|-----|
| `github.com/bsv-blockchain/go-sdk` | `transaction.MerklePath`, `PathElement`, `Combine`, `chainhash.Hash` |
| `github.com/dgraph-io/badger/v4` | Disk-backed LSM-tree key-value store for all miners' subtree data |

The merkle tree implementation (`internal/subtree`) uses only Go's standard `crypto/sha256` and the go-sdk `chainhash` type.  There is no dependency on `go-bt` or `go-subtree`.
