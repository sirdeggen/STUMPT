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
3. A lightweight **TokenSubtreeIndex** records which tokens have txids in each subtree and their leaf positions (4 bytes/entry).
4. When the block is found, a small **top tree** (one leaf per subtree root) is built and proofs are computed **just-in-time** from the cached merkle stores.
5. Each subscribing business receives a single **compound BUMP** that covers all of their transactions at once, assembled via a streaming pipeline.

This repo simulates that pipeline end-to-end and measures the timing of each phase.

---

## System requirements

The harness auto-detects your system's RAM and scales the default test to use ~80% of physical memory with 1M-leaf subtrees.

```
╔══════════════════════════════════════════════════════════════════════════╗
║                     STUMPT System Requirements                          ║
╠═══════════════╦═══════════════╦═══════════════╦══════════════════════════╣
║   Total TXIDs ║   Subtrees    ║   Est. RAM    ║   Min System RAM        ║
╠═══════════════╬═══════════════╬═══════════════╬══════════════════════════╣
║            2M ║       2 × 1M ║        4.3 GB ║        5.4 GB             ║
║            4M ║       4 × 1M ║        4.6 GB ║        5.8 GB             ║
║           10M ║      10 × 1M ║        5.5 GB ║        6.9 GB             ║
║           21M ║      20 × 1M ║        7.1 GB ║        8.9 GB             ║
║           42M ║      40 × 1M ║       10.2 GB ║       12.7 GB             ║
║           63M ║      60 × 1M ║       13.3 GB ║       16.6 GB             ║
║          105M ║     100 × 1M ║       19.4 GB ║       24.3 GB             ║
║          157M ║     150 × 1M ║       27.1 GB ║       33.9 GB             ║
║          315M ║     300 × 1M ║       50.3 GB ║       62.9 GB             ║
║          629M ║     600 × 1M ║       96.6 GB ║      120.7 GB             ║
╚═══════════════╩═══════════════╩═══════════════╩══════════════════════════╝

Memory model: 158 B/txid + 4 GB overhead
```

**158 B/txid** breaks down as:
- 64 B — miner-0 subtree data (leaves + merkle store, kept in memory)
- 90 B — in-memory TxID index (`map[chainhash.Hash]string`)
- 4 B — TokenSubtreeIndex entry (`int32` local leaf position)

**4 GB overhead** covers BadgerDB (disk-backed storage for non-miner-0 data), Go runtime, BUMP assembly buffers, and OS overhead.

If your machine doesn't have enough RAM for the requested configuration, the harness exits with a clear error and prints this table. Use `-max-memory` to set a specific budget, or `-requirements` to just print the table.

---

## Architecture

Three components run in a single process:

```
  Generator ──POST /watch──► Merkle Service ──POST (raw BUMP)──► Callback Server
```

In **direct mode** (`-direct` flag), the generator bypasses HTTP and calls the registry directly, enabling large-scale runs (millions of txids) that would be bottlenecked by HTTP/JSON overhead.

### Storage strategy

The system uses a hybrid in-memory / disk-backed approach to balance speed and memory:

| Data | Storage | Why |
|------|---------|-----|
| Miner-0 subtree leaves + stores | **In-memory** | Hot path — needed for JIT proof computation during BUMP assembly |
| Miners 1+ subtree data | **BadgerDB (disk)** | Cold — only needed if a non-miner-0 block wins |
| TxID → token index | **In-memory** | Accessed on every txid arrival for O(1) lookup |
| TokenSubtreeIndex | **In-memory** | Lightweight (4B/entry), accessed during BUMP assembly |
| Buffered txid lists | **BadgerDB (disk)** | Per-token txid lists, batched writes |

### Generator (`internal/generator`)
- Produces `HASHES_PER_BLOCK` random 32-byte hashes (mock txids).
- **HTTP mode:** Submits at a rate of `HASHES_PER_BLOCK / duration` per second.
- **Direct mode:** Submits as fast as possible (bypasses HTTP), achieving 1M+ txids/sec.
- Each submission picks one of N callback tokens round-robin, simulating N distinct submitting businesses.

### Merkle Service (`internal/merkleservice`)
- Receives `POST /watch` with `{ txid, callback: { url, token } }` (or direct calls in `-direct` mode).
- Every `HASHES_PER_SUBTREE` txids received, **seals a subtree**:
  - Builds N miners' versions of the subtree with deterministically jittered ordering.
  - Parallel merkle tree building for large subtrees (goroutines for levels with 4096+ pairs).
  - Records token leaf positions in the **TokenSubtreeIndex** (4B per entry).
  - Keeps miner-0 subtrees in memory; evicts miners 1+ to BadgerDB.
- When `HASHES_PER_BLOCK` txids are received, **finalises the block**:
  - Performs coinbase replacement + subtree-0 reseal.
  - Builds the top tree from subtree roots.
  - **JIT proof computation:** assembles compound BUMPs from cached miner-0 merkle stores using the TokenSubtreeIndex to find leaf positions.
  - Streams BUMPs via a build→deliver pipeline (no accumulation of all BUMPs in memory).

### Callback Server (`internal/callback`)
- Receives the raw BUMP binary POSTs.
- Logs token, payload size, and end-to-end latency from block announcement to delivery.

### Subtree / Merkle Engine (`internal/subtree`)
- Implements `BuildMerkleStore` and `GetMerkleProof` using `go-sdk/chainhash` only.
- Parallel merkle tree building: spawns goroutines for levels with 4096+ hash pairs.
- `HashPair` is byte-identical to `transaction.MerkleTreeParent` in go-sdk, verified by test.

### TokenSubtreeIndex (`internal/merkleservice`)
- Lightweight replacement for the full STUMP store.
- Maps `(token, subtreeIdx) → []int32` (local leaf positions within the subtree).
- 4 bytes per indexed txid vs ~860 bytes for pre-computed full proofs.
- Thread-safe with `sync.RWMutex`.
- BUMP assembly uses these positions to compute proofs JIT from cached merkle stores.

### Disk Store (`internal/diskstore`)
- BadgerDB v4 LSM-tree for persistent key-value storage.
- `MinerSubtreeStore`: saves/loads subtree leaves and merkle stores for non-miner-0 miners.
- `BufferedTxidList`: batched writes (1000 entries per flush) for per-token txid lists.
- `BufferedTxIDIndex`: batched writes for txid→token mappings on disk (backup to in-memory index).

---

## Default configuration

The defaults are auto-detected based on your system's available RAM:

| Parameter | Default | Notes |
|-----------|---------|-------|
| `HASHES_PER_SUBTREE` | 1 048 576 (1M) | subtree height = 20 |
| `HASHES_PER_BLOCK` | auto-detected | fills ~80% of system RAM with 1M-leaf subtrees |
| Num miners | 3 | competing subtree orderings |
| Num businesses | 1 000 | distinct callback tokens |
| Test duration | 10 s | ignored in `-direct` mode |
| Mock block height | 800 000 | stamped in every BUMP |

For example, on a 36 GB machine the default is 160 subtrees × 1M = **167M txids** (~28.7 GB estimated).
On a 16 GB machine: ~76 subtrees × 1M = **~80M txids**.

---

## Prerequisites

- Go 1.24 or later (`go version`)
- Ports **:18080** (merkle service) and **:13000** (callback server) available

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
./harness -direct
```

The harness auto-detects your system's RAM and runs the largest test that fits in ~80% of physical memory with 1M-leaf subtrees. Structured JSON logs stream to stdout. When all BUMPs are delivered, a summary table is printed:

```
╔══════════════════════════════════════════╗
║          STUMPT FINAL SUMMARY            ║
╠══════════════════════════════════════════╣
║  Elapsed:                         7.302s  ║
║  Txids submitted:                2097152  ║
║  Actual rate:                287215.9/s  ║
╠══════════════════════════════════════════╣
║  Subtrees sealed:                      2  ║
║  Avg seal time:                 50.45ms  ║
║  Proof pre-computations:               2  ║
║  Avg proof time:               202.36ms  ║
╠══════════════════════════════════════════╣
║  Coinbase reseal:              164.42ms  ║
║  Top tree build:                 0.00ms  ║
║  BUMP assembly ( 100tok):     1329.30ms  ║
║  Callbacks delivered:                200  ║
║  Avg callback time:            391.34ms  ║
║  Avg BUMP size:               6673217 B  ║
║  Total BUMP bytes:         1334643448 B  ║
╚══════════════════════════════════════════╝
```

### 3. Constrained memory budget

```bash
# Limit to 20 GB peak — auto-scales to fit
./harness -direct -max-memory 20
```

This recalculates the number of subtrees to fit within 20 GB (e.g. 100 subtrees × 1M = 105M txids on any machine).

### 4. Print system requirements

```bash
./harness -requirements
```

Prints the requirements table and exits without running any test.

### 5. Explicit scale

```bash
# 2M txids: 2 subtrees × 1M leaves, 100 businesses
./harness -direct -hashes-per-block 2097152 -hashes-per-subtree 1048576 -businesses 100

# 60M txids: 60 subtrees × 1M leaves, 1000 businesses (needs ~17 GB)
./harness -direct -hashes-per-block 62914560 -hashes-per-subtree 1048576 -businesses 1000
```

If the requested configuration exceeds available memory, the harness exits with an error:
```
ERROR: estimated memory 19.4 GB exceeds budget 10.0 GB (105M txids × 158 B/txid + 4.0 GB overhead)
Reduce -hashes-per-block or -hashes-per-subtree, or increase -max-memory
```

### 6. HTTP mode (small-scale / paced submission)

```bash
# 1024 txids over 10 seconds, HTTP submission
./harness -hashes-per-block 1024 -hashes-per-subtree 64
```

### 7. All CLI flags

```
-hashes-per-block   int      Total txids per simulated block    (default: auto-detected from RAM)
-hashes-per-subtree int      Txids per subtree                  (default 1048576)
-miners             int      Number of competing miners         (default 3)
-businesses         int      Distinct callback tokens           (default 1000)
-duration           duration Total test duration                (default 10s; ignored in -direct mode)
-merkle-addr        string   Merkle service listen address      (default :18080)
-callback-addr      string   Callback server listen address     (default :13000)
-direct                      Bypass HTTP for txid submission    (fast path for large-scale runs)
-dump-bump          string   Write first assembled BUMP as hex to this file (optional)
-data-dir           string   BadgerDB data directory            (empty = temp dir)
-max-memory         float    Peak memory budget in GB           (default: 80% of system RAM)
-requirements                Print system requirements table and exit
```

`hashes-per-block` must be divisible by `hashes-per-subtree`.

---

## Running the tests

The merkle engine and STUMP index have full unit + integration test suites.

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

# STUMP index benchmarks
go test -bench=. -benchmem ./internal/stump/

# BUMP assembly benchmarks (isolated from HTTP)
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
| `stump` | `TestXORKeySymmetry` | XOR(a, b) == XOR(b, a) |
| `stump` | `TestXORKeySelfInverse` | XOR(XOR(a, b), b) == a |
| `stump` | `TestDiscoverEndToEnd` | Full STUMP lifecycle: register, seal, discover |
| `stump` | `TestStoreAppendAndGet` | Basic store operations |
| `stump` | `TestTokenRegistry` | Token registration and hash lookup |

---

## What the numbers tell you

The key insight this harness measures is the **work distribution**:

- **During the block interval:** subtree sealing + merkle tree building runs continuously. Each subtree seal takes `~50 ms` for 1M leaves (with parallel hash computation). This work is spread evenly across all subtree boundaries throughout the inter-block interval.
- **At block found:** only the coinbase reseal (~164 ms for 1M leaves), top-tree build (microseconds), and JIT BUMP assembly need to happen before delivery. The streaming pipeline assembles and delivers BUMPs concurrently — no accumulation of all BUMPs in memory.
- **Per-business BUMP size** reflects how many txids that business submitted. Businesses with more txids get larger but more compact compound BUMPs (intermediate hashes are pruned).

---

## Dependencies

| Package | Use |
|---------|-----|
| `github.com/bsv-blockchain/go-sdk` | `transaction.MerklePath`, `PathElement`, `Combine`, `chainhash.Hash` |
| `github.com/dgraph-io/badger/v4` | Disk-backed LSM-tree key-value store for non-miner-0 subtree data |

The merkle tree implementation (`internal/subtree`) uses only Go's standard `crypto/sha256` and the go-sdk `chainhash` type.  There is no dependency on `go-bt` or `go-subtree`.
