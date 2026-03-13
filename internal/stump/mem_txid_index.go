package stump

import (
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
)

// MemTxIDIndex is an in-memory TxIDIndexer backed by a simple map.
// It trades RAM for speed — no disk I/O on Get.
type MemTxIDIndex struct {
	mu sync.RWMutex
	m  map[chainhash.Hash]string
}

// NewMemTxIDIndex creates an in-memory txid→token index.
func NewMemTxIDIndex() *MemTxIDIndex {
	return &MemTxIDIndex{
		m: make(map[chainhash.Hash]string),
	}
}

func (idx *MemTxIDIndex) Set(txid chainhash.Hash, token string) {
	idx.mu.Lock()
	idx.m[txid] = token
	idx.mu.Unlock()
}

func (idx *MemTxIDIndex) Get(txid chainhash.Hash) (string, bool) {
	idx.mu.RLock()
	tok, ok := idx.m[txid]
	idx.mu.RUnlock()
	return tok, ok
}

func (idx *MemTxIDIndex) BatchGet(txids []chainhash.Hash) []string {
	result := make([]string, len(txids))
	idx.mu.RLock()
	for i, txid := range txids {
		result[i] = idx.m[txid]
	}
	idx.mu.RUnlock()
	return result
}

func (idx *MemTxIDIndex) Len() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.m)
}
