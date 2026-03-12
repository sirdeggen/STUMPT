package diskstore

import (
	"encoding/binary"

	badger "github.com/dgraph-io/badger/v4"
)

// MinerSubtreeStore persists MinerSubtree Leaves and Store arrays to disk
// so they can be evicted from RAM after sealing.
type MinerSubtreeStore struct {
	db *DB
}

// NewMinerSubtreeStore creates a new MinerSubtreeStore backed by the given DB.
func NewMinerSubtreeStore(db *DB) *MinerSubtreeStore {
	return &MinerSubtreeStore{db: db}
}

// minerSubtreeKey builds a key:
// 'm' (1 byte) + minerIdx (4 bytes BE) + subtreeIdx (4 bytes BE) + type (1 byte)
func minerSubtreeKey(minerIdx, subtreeIdx int, typ byte) []byte {
	key := make([]byte, 1+4+4+1)
	key[0] = 'm'
	binary.BigEndian.PutUint32(key[1:5], uint32(minerIdx))
	binary.BigEndian.PutUint32(key[5:9], uint32(subtreeIdx))
	key[9] = typ
	return key
}

// Save writes both leaves and store for the given miner/subtree in a single transaction.
func (s *MinerSubtreeStore) Save(minerIdx, subtreeIdx int, leaves, store []byte) error {
	lKey := minerSubtreeKey(minerIdx, subtreeIdx, 'L')
	sKey := minerSubtreeKey(minerIdx, subtreeIdx, 'S')

	return s.db.BadgerDB().Update(func(txn *badger.Txn) error {
		if err := txn.Set(lKey, leaves); err != nil {
			return err
		}
		return txn.Set(sKey, store)
	})
}

// Load reads leaves and store for the given miner/subtree.
// Returns ok=false if either key is not found.
func (s *MinerSubtreeStore) Load(minerIdx, subtreeIdx int) (leaves, store []byte, ok bool) {
	lKey := minerSubtreeKey(minerIdx, subtreeIdx, 'L')
	sKey := minerSubtreeKey(minerIdx, subtreeIdx, 'S')

	err := s.db.BadgerDB().View(func(txn *badger.Txn) error {
		item, err := txn.Get(lKey)
		if err != nil {
			return err
		}
		leaves, err = item.ValueCopy(nil)
		if err != nil {
			return err
		}

		item, err = txn.Get(sKey)
		if err != nil {
			return err
		}
		store, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		return nil, nil, false
	}
	return leaves, store, true
}
