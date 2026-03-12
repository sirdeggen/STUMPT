package diskstore

import (
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

func randomHashPtr(t testing.TB) *chainhash.Hash {
	t.Helper()
	h := randomHash(t)
	return &h
}

func TestMarshalUnmarshalEntry(t *testing.T) {
	e := &stump.Entry{
		TxID:       randomHash(t),
		SubtreeIdx: 7,
		LocalIdx:   42,
		GlobalIdx:  999,
		SiblingPath: []*chainhash.Hash{
			randomHashPtr(t),
			randomHashPtr(t),
			randomHashPtr(t),
		},
	}

	data := MarshalEntry(e)
	got, err := UnmarshalEntry(data)
	if err != nil {
		t.Fatal(err)
	}

	if got.TxID != e.TxID {
		t.Error("TxID mismatch")
	}
	if got.SubtreeIdx != e.SubtreeIdx {
		t.Error("SubtreeIdx mismatch")
	}
	if got.LocalIdx != e.LocalIdx {
		t.Error("LocalIdx mismatch")
	}
	if got.GlobalIdx != e.GlobalIdx {
		t.Error("GlobalIdx mismatch")
	}
	if len(got.SiblingPath) != len(e.SiblingPath) {
		t.Fatalf("SiblingPath length mismatch: %d vs %d", len(got.SiblingPath), len(e.SiblingPath))
	}
	for i := range e.SiblingPath {
		if *got.SiblingPath[i] != *e.SiblingPath[i] {
			t.Errorf("SiblingPath[%d] mismatch", i)
		}
	}
}

func TestMarshalUnmarshalEntryNoSiblings(t *testing.T) {
	e := &stump.Entry{
		TxID:        randomHash(t),
		SubtreeIdx:  0,
		LocalIdx:    0,
		GlobalIdx:   0,
		SiblingPath: nil,
	}

	data := MarshalEntry(e)
	got, err := UnmarshalEntry(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SiblingPath) != 0 {
		t.Errorf("expected 0 siblings, got %d", len(got.SiblingPath))
	}
}

func TestMarshalUnmarshalEntries(t *testing.T) {
	entries := []*stump.Entry{
		{
			TxID:       randomHash(t),
			SubtreeIdx: 1,
			LocalIdx:   2,
			GlobalIdx:  3,
			SiblingPath: []*chainhash.Hash{
				randomHashPtr(t),
			},
		},
		{
			TxID:        randomHash(t),
			SubtreeIdx:  10,
			LocalIdx:    20,
			GlobalIdx:   30,
			SiblingPath: nil,
		},
		{
			TxID:       randomHash(t),
			SubtreeIdx: 100,
			LocalIdx:   200,
			GlobalIdx:  300,
			SiblingPath: []*chainhash.Hash{
				randomHashPtr(t),
				randomHashPtr(t),
			},
		},
	}

	data := MarshalEntries(entries)
	got, err := UnmarshalEntries(data)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(entries) {
		t.Fatalf("count mismatch: %d vs %d", len(got), len(entries))
	}
	for i := range entries {
		if got[i].TxID != entries[i].TxID {
			t.Errorf("entry[%d] TxID mismatch", i)
		}
		if got[i].SubtreeIdx != entries[i].SubtreeIdx {
			t.Errorf("entry[%d] SubtreeIdx mismatch", i)
		}
		if len(got[i].SiblingPath) != len(entries[i].SiblingPath) {
			t.Errorf("entry[%d] SiblingPath length mismatch", i)
		}
	}
}

func TestMarshalUnmarshalEntriesEmpty(t *testing.T) {
	data := MarshalEntries(nil)
	got, err := UnmarshalEntries(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 entries, got %d", len(got))
	}
}

func TestMarshalUnmarshalHashes(t *testing.T) {
	hashes := []chainhash.Hash{
		randomHash(t),
		randomHash(t),
		randomHash(t),
		randomHash(t),
	}

	data := MarshalHashes(hashes)
	got, err := UnmarshalHashes(data)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != len(hashes) {
		t.Fatalf("count mismatch: %d vs %d", len(got), len(hashes))
	}
	for i := range hashes {
		if got[i] != hashes[i] {
			t.Errorf("hash[%d] mismatch", i)
		}
	}
}

func TestMarshalUnmarshalHashesEmpty(t *testing.T) {
	data := MarshalHashes(nil)
	got, err := UnmarshalHashes(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 hashes, got %d", len(got))
	}
}

func TestUnmarshalEntryTruncated(t *testing.T) {
	_, err := UnmarshalEntry([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for truncated data")
	}
}

func TestUnmarshalHashesTruncated(t *testing.T) {
	_, err := UnmarshalHashes([]byte{1, 2})
	if err == nil {
		t.Error("expected error for truncated data")
	}
}
