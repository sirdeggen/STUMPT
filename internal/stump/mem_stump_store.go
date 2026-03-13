package stump

import "sync"

// MemStumpStore is an in-memory StumpStore backed by a map of slices.
// Trades RAM for speed — no disk I/O on Append/Get.
type MemStumpStore struct {
	mu sync.RWMutex
	m  map[Key][]*Entry
	n  int // total entries across all keys
}

// NewMemStumpStore creates an in-memory stump store.
func NewMemStumpStore() *MemStumpStore {
	return &MemStumpStore{
		m: make(map[Key][]*Entry),
	}
}

func (s *MemStumpStore) Append(key Key, e *Entry) {
	s.mu.Lock()
	s.m[key] = append(s.m[key], e)
	s.n++
	s.mu.Unlock()
}

func (s *MemStumpStore) AppendBatch(key Key, entries []*Entry) {
	s.mu.Lock()
	s.m[key] = append(s.m[key], entries...)
	s.n += len(entries)
	s.mu.Unlock()
}

func (s *MemStumpStore) Get(key Key) []*Entry {
	s.mu.RLock()
	result := s.m[key]
	s.mu.RUnlock()
	return result
}

func (s *MemStumpStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.m)
}
