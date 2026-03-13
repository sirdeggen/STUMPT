# CLAUDE.md

## What is this repo?

STUMPT (**S**ub**T**ree **U**nified **M**erkle **P**ath **T**esting) is a performance test harness for incremental compound BUMP (BRC-74) generation at block-scale BSV transaction volumes. It simulates the full pipeline from txid generation through subtree sealing to BUMP assembly for 1000+ businesses at 200M+ transactions.

This is a **benchmark harness**, not a production service. There are no HTTP endpoints, no real blockchain interaction. Everything runs in a single process with BadgerDB for disk persistence.

## Key concepts

- **Subtree**: Fixed-size partition of block transactions (default 1M leaves). Each has its own merkle tree built during the inter-block interval.
- **STUMP fragment**: Pre-computed per-token per-subtree BUMP proof data (~82 KB). Extracted during Phase 2 when the merkle store is in memory.
- **Token**: Business identifier derived from txid hash: `uint64(txid[:8]) % NumBusinesses`. Random, uniform, order-independent.
- **Compound BUMP**: Single BRC-74 proof covering all of a business's txids. Shared intermediate nodes pruned.

## Pipeline phases

1. **Phase 1** — Generate random txids (`cmd/harness/main.go`)
2. **Phase 2** — Seal subtrees to disk, build TokenSubtreeIndex, extract STUMP fragments (`internal/merkleservice/registry.go`)
3. **Phase 4** — Block found: coinbase reseal, load fragments, assemble BUMPs (`internal/merkleservice/bump.go`)

## Package layout

```
cmd/harness/          — CLI entry point, phase orchestration
internal/
  config/             — CLI flags, memory budget, auto-detection
  diskstore/          — BadgerDB persistence layer
    db.go             — DB open/close
    miner_subtree.go  — Leaves+store storage ('m' prefix keys)
    fragment.go       — STUMP fragment storage ('f' prefix keys)
    stump_store.go    — Legacy stump entry storage ('s' prefix, not in hot path)
    encoding.go       — Marshal/unmarshal helpers
  merkleservice/      — Core business logic
    bump.go           — BUMP assembly: fast path, fragment path, JIT path, serialization
    registry.go       — Phase 2 sealing, Phase 4 finalization, token assignment
    token_subtree_index.go — In-memory (token, subtreeIdx) -> []localIdx index
    types.go          — Shared types (BlockFinalizedEvent, SubtreeProof, etc.)
  metrics/            — Timing/size collectors
  stump/              — Stump entry types
  subtree/            — Merkle tree engine (BuildMerkleStore, GetMerkleProof, HashPair)
```

## Important files for performance work

- `internal/merkleservice/bump.go` — The hot path. Contains three assembly implementations:
  - `assembleTokenBUMPFromFragments()` — Fragment-based (production path)
  - `assembleTokenBUMPFast()` — Bitset-based JIT (fallback)
  - `assembleTokenBUMP()` — Legacy map-based (kept for test comparison)
  - `serializeBUMP()` — Custom BRC-74 binary writer
  - `extractSubtreeFragment()` — Fragment extraction (used in Phase 2)
  - `MarshalFragment()` / `UnmarshalFragment()` — Fragment serialization

- `internal/merkleservice/registry.go` — Phase 2 sealing + Phase 4 finalization
- `internal/subtree/subtree.go` — Merkle tree construction (parallel SHA256d)

## Storage

All persistent data is in BadgerDB with key-prefix namespacing:
- `'m'` — Raw subtree data (leaves + merkle store) per (miner, subtree)
- `'f'` — STUMP fragments per (miner, token, subtree)

## Running

```bash
go build -o harness ./cmd/harness/
./harness                              # auto-detect scale from RAM
./harness -hashes-per-block 2097152 -businesses 100  # explicit
./harness -requirements                # print requirements table
```

## Testing

```bash
go test ./...                          # all tests
go test -bench=. -benchmem ./internal/merkleservice/  # BUMP assembly benchmarks
go test -bench=. -benchmem ./internal/subtree/        # merkle engine benchmarks
```

Key correctness tests:
- `TestFastVsLegacyBUMP` — Bitset path matches legacy map-based path byte-for-byte
- `TestFragmentVsJIT` — Fragment-based assembly matches JIT assembly byte-for-byte

## Common tasks

**Changing token assignment**: `registry.go` around line 103. Currently hash-derived from txid.

**Changing fragment format**: `bump.go` — `MarshalFragment()` and `UnmarshalFragment()`. Entry is 41 bytes: 8B offset + 1B flags + 32B hash.

**Adding a new storage type**: Follow the pattern in `diskstore/fragment.go` — pick a new prefix byte, define key format, implement Save/Load/SaveBatch.

**Benchmarking a change**: Run the medium harness test and compare Phase 4 BUMP assembly time:
```bash
./harness -hashes-per-block 2097152 -hashes-per-subtree 1048576 -businesses 100
```
