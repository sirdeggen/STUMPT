package diskstore

import (
	"encoding/binary"
	"log/slog"

	badger "github.com/dgraph-io/badger/v4"
)

// FragmentStore persists pre-computed per-token STUMP fragments to disk.
// Each fragment contains the subtree-level BUMP entries (levels 0..subtreeHeight-1)
// for a single token in a single subtree.
//
// Key: 'f' (1B) | minerIdx (4B BE) | tokenIdx (4B BE) | subtreeIdx (4B BE) = 13 bytes
// Value: flat-encoded bumpEntry data per level (see MarshalFragment/UnmarshalFragment).
type FragmentStore struct {
	db *DB
}

// NewFragmentStore creates a new FragmentStore backed by the given DB.
func NewFragmentStore(db *DB) *FragmentStore {
	return &FragmentStore{db: db}
}

// fragmentKey builds the 13-byte key for a fragment.
func fragmentKey(minerIdx, tokenIdx, subtreeIdx int) []byte {
	key := make([]byte, 13)
	key[0] = 'f'
	binary.BigEndian.PutUint32(key[1:5], uint32(minerIdx))
	binary.BigEndian.PutUint32(key[5:9], uint32(tokenIdx))
	binary.BigEndian.PutUint32(key[9:13], uint32(subtreeIdx))
	return key
}

// Save writes a single fragment.
func (fs *FragmentStore) Save(minerIdx, tokenIdx, subtreeIdx int, data []byte) error {
	key := fragmentKey(minerIdx, tokenIdx, subtreeIdx)
	return fs.db.BadgerDB().Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
}

// SaveBatch writes multiple fragments using a WriteBatch for efficiency.
// fragments maps tokenIdx → marshaled fragment bytes.
func (fs *FragmentStore) SaveBatch(minerIdx, subtreeIdx int, fragments map[int][]byte) error {
	if len(fragments) == 0 {
		return nil
	}

	wb := fs.db.BadgerDB().NewWriteBatch()
	count := 0
	for tokenIdx, data := range fragments {
		key := fragmentKey(minerIdx, tokenIdx, subtreeIdx)
		if err := wb.Set(key, data); err != nil {
			wb.Cancel()
			return err
		}
		count++
		if count >= 50_000 {
			if err := wb.Flush(); err != nil {
				return err
			}
			wb = fs.db.BadgerDB().NewWriteBatch()
			count = 0
		}
	}

	if count > 0 {
		return wb.Flush()
	}
	return nil
}

// Load reads a single fragment. Returns nil, false if not found.
func (fs *FragmentStore) Load(minerIdx, tokenIdx, subtreeIdx int) ([]byte, bool) {
	key := fragmentKey(minerIdx, tokenIdx, subtreeIdx)
	var data []byte

	err := fs.db.BadgerDB().View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		data, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		if err != badger.ErrKeyNotFound {
			slog.Error("FragmentStore.Load failed", "error", err)
		}
		return nil, false
	}
	return data, true
}
