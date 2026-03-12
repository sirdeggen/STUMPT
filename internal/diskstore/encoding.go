package diskstore

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/stumpt/internal/stump"
)

const hashSize = 32

// MarshalEntry encodes an Entry as:
// [32B TxID][4B SubtreeIdx LE][4B LocalIdx LE][4B GlobalIdx LE][4B numSiblings LE][32B * siblings...]
func MarshalEntry(e *stump.Entry) []byte {
	numSiblings := len(e.SiblingPath)
	buf := make([]byte, hashSize+4+4+4+4+hashSize*numSiblings)
	off := 0

	copy(buf[off:], e.TxID[:])
	off += hashSize

	binary.LittleEndian.PutUint32(buf[off:], uint32(e.SubtreeIdx))
	off += 4
	binary.LittleEndian.PutUint32(buf[off:], uint32(e.LocalIdx))
	off += 4
	binary.LittleEndian.PutUint32(buf[off:], uint32(e.GlobalIdx))
	off += 4
	binary.LittleEndian.PutUint32(buf[off:], uint32(numSiblings))
	off += 4

	for _, h := range e.SiblingPath {
		copy(buf[off:], h[:])
		off += hashSize
	}
	return buf
}

// UnmarshalEntry decodes an Entry from the format produced by MarshalEntry.
func UnmarshalEntry(data []byte) (*stump.Entry, error) {
	const headerSize = hashSize + 4 + 4 + 4 + 4
	if len(data) < headerSize {
		return nil, errors.New("diskstore: entry data too short")
	}

	e := &stump.Entry{}
	off := 0

	copy(e.TxID[:], data[off:off+hashSize])
	off += hashSize

	e.SubtreeIdx = int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	e.LocalIdx = int(binary.LittleEndian.Uint32(data[off:]))
	off += 4
	e.GlobalIdx = int(binary.LittleEndian.Uint32(data[off:]))
	off += 4

	numSiblings := int(binary.LittleEndian.Uint32(data[off:]))
	off += 4

	need := off + numSiblings*hashSize
	if len(data) < need {
		return nil, fmt.Errorf("diskstore: need %d bytes for %d siblings, have %d", need, numSiblings, len(data))
	}

	e.SiblingPath = make([]*chainhash.Hash, numSiblings)
	for i := 0; i < numSiblings; i++ {
		h := new(chainhash.Hash)
		copy(h[:], data[off:off+hashSize])
		off += hashSize
		e.SiblingPath[i] = h
	}
	return e, nil
}

// MarshalEntries encodes a slice of entries as:
// [4B count][4B len + entry bytes]...
func MarshalEntries(entries []*stump.Entry) []byte {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, uint32(len(entries)))

	for _, e := range entries {
		eb := MarshalEntry(e)
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(eb)))
		buf = append(buf, lenBuf[:]...)
		buf = append(buf, eb...)
	}
	return buf
}

// UnmarshalEntries decodes a slice of entries from the format produced by MarshalEntries.
func UnmarshalEntries(data []byte) ([]*stump.Entry, error) {
	if len(data) < 4 {
		return nil, errors.New("diskstore: entries data too short")
	}
	count := int(binary.LittleEndian.Uint32(data[:4]))
	off := 4

	entries := make([]*stump.Entry, 0, count)
	for i := 0; i < count; i++ {
		if off+4 > len(data) {
			return nil, fmt.Errorf("diskstore: truncated entry length at index %d", i)
		}
		eLen := int(binary.LittleEndian.Uint32(data[off:]))
		off += 4
		if off+eLen > len(data) {
			return nil, fmt.Errorf("diskstore: truncated entry data at index %d", i)
		}
		e, err := UnmarshalEntry(data[off : off+eLen])
		if err != nil {
			return nil, fmt.Errorf("diskstore: entry %d: %w", i, err)
		}
		entries = append(entries, e)
		off += eLen
	}
	return entries, nil
}

// MarshalHashes encodes a slice of hashes as:
// [4B count][32B hash]...
func MarshalHashes(hashes []chainhash.Hash) []byte {
	buf := make([]byte, 4+hashSize*len(hashes))
	binary.LittleEndian.PutUint32(buf, uint32(len(hashes)))
	for i, h := range hashes {
		copy(buf[4+i*hashSize:], h[:])
	}
	return buf
}

// UnmarshalHashes decodes a slice of hashes from the format produced by MarshalHashes.
func UnmarshalHashes(data []byte) ([]chainhash.Hash, error) {
	if len(data) < 4 {
		return nil, errors.New("diskstore: hashes data too short")
	}
	count := int(binary.LittleEndian.Uint32(data[:4]))
	need := 4 + count*hashSize
	if len(data) < need {
		return nil, fmt.Errorf("diskstore: need %d bytes for %d hashes, have %d", need, count, len(data))
	}
	hashes := make([]chainhash.Hash, count)
	for i := 0; i < count; i++ {
		copy(hashes[i][:], data[4+i*hashSize:])
	}
	return hashes, nil
}
