package finalindex

import (
	"errors"
	"fmt"
	"sort"
)

// EndDocID is larger than every representable on-disk docID. Using uint64
// also makes NextGEQ(current+1) safe when current is math.MaxUint32
const EndDocID uint64 = 1 << 32

// ErrTermNotFound reports a term absent from the index lexicon
var ErrTermNotFound = errors.New("term not found")

// List is a forward-only cursor.
type List struct {
	reader *Reader
	entry  Entry // Lexicon entry

	descriptors []BlockDescriptor // Block descriptors for the current term
	block       int               // Block number
	position    int               // Position inside current block

	docs  []uint32
	freqs []uint32

	exhausted bool
	closed    bool
	failure   error
}

// OpenList finds a term in the lexicon without reading any posting payload
func (r *Reader) OpenList(term string) (*List, error) {
	entry, found := r.lexicon[term]
	if !found {
		return nil, fmt.Errorf("%w: %q", ErrTermNotFound, term)
	}
	descriptors, err := r.BlockDescriptors(entry)
	if err != nil {
		return nil, fmt.Errorf("%s descriptors: %w", term, err)
	}
	return &List{reader: r, entry: entry, descriptors: descriptors, block: -1}, nil
}

// DocFrequency returns the number of documents containing this term
func (list *List) DocFrequency() uint64 { return list.entry.DocFrequency }

// NextGEQ returns the first remaining docID >= target, or EndDocID
// It keeps the current posting when target <= current, and never moves backward
// Only this operation reads and decompresses document chunks
func (list *List) NextGEQ(target uint64) (uint64, error) {
	if list.closed {
		return EndDocID, errors.New("list is closed")
	}
	if list.failure != nil {
		return EndDocID, list.failure
	}
	if list.exhausted {
		return EndDocID, nil
	}
	if target >= EndDocID {
		list.exhausted = true
		return EndDocID, nil
	}
	if list.block >= 0 && uint64(list.docs[list.position]) >= target {
		return uint64(list.docs[list.position]), nil
	}
	block := list.block
	if block < 0 || uint64(list.descriptors[block].LastDocID) < target {
		start := block + 1
		block = start + sort.Search(len(list.descriptors)-start, func(i int) bool {
			return uint64(list.descriptors[start+i].LastDocID) >= target
		})
		if block == len(list.descriptors) {
			list.exhausted = true
			return EndDocID, nil
		}
		docs, err := list.reader.readDocumentBlock(list.descriptors[block])
		if err == nil && block > 0 && docs[0] <= list.descriptors[block-1].LastDocID {
			err = errors.New("posting blocks overlap")
		}
		if err != nil {
			list.failure = fmt.Errorf("%s block %d: %w", list.entry.Term, block, err)
			return EndDocID, list.failure
		}
		// Set frequencies to nil to discard them since they belong to the old block
		list.block, list.position, list.docs, list.freqs = block, 0, docs, nil
	}
	remaining := list.docs[list.position:]
	list.position += sort.Search(len(remaining), func(i int) bool { return uint64(remaining[i]) >= target })
	return uint64(list.docs[list.position]), nil
}

// GetFreq returns the current posting's frequency. It reads and decodes the
// frequency chunk once per visited block, only if scoring actually requests it
func (list *List) GetFreq() (uint32, error) {
	if list.closed {
		return 0, errors.New("list is closed")
	}
	if list.failure != nil {
		return 0, list.failure
	}
	if list.block < 0 || list.exhausted {
		return 0, errors.New("list has no current posting")
	}
	if list.freqs == nil {
		freqs, err := list.reader.readFrequencyBlock(list.descriptors[list.block])
		if err != nil {
			list.failure = fmt.Errorf("%s frequencies: %w", list.entry.Term, err)
			return 0, list.failure
		}
		list.freqs = freqs
	}
	return list.freqs[list.position], nil
}

// Close releases this cursor's buffers. Shared index files remain open
func (list *List) Close() {
	list.docs, list.freqs, list.descriptors, list.reader = nil, nil, nil, nil
	list.closed = true
}
