package finalindex

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Entry is one row of lexicon.tsv. Offsets are absolute bytes in blocks.bin
type Entry struct {
	Term                                   string
	DocFrequency, BlocksOffset, BlockCount uint64
}

// Parse a given line of the lexicon.tsv file
func ParseEntry(line string) (Entry, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 4 {
		return Entry{}, fmt.Errorf("expected four lexicon columns")
	}

	lexicon_entry := Entry{Term: fields[0]}
	var err error
	dst := []*uint64{&lexicon_entry.DocFrequency, &lexicon_entry.BlocksOffset, &lexicon_entry.BlockCount}
	for i, p := range dst {
		*p, err = strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return Entry{}, err
		}
	}

	if lexicon_entry.DocFrequency == 0 || lexicon_entry.BlockCount == 0 || lexicon_entry.BlocksOffset < 4 || lexicon_entry.BlocksOffset%24 != 4 {
		return Entry{}, fmt.Errorf("invalid lexicon entry")
	}

	return lexicon_entry, nil
}

// Reader owns the shared index files and lexicon used by list cursors
type Reader struct {
	posts, blocks, pages *os.File
	lexicon              map[string]Entry
}

// Open validates the index, opens its binary files, and loads its lexicon
func Open(dir string) (r *Reader, err error) {
	if _, e := os.Stat(filepath.Join(dir, "INCOMPLETE")); e == nil {
		return nil, fmt.Errorf("index build is incomplete")
	} else if !os.IsNotExist(e) {
		return nil, e
	}

	r = &Reader{}
	opened := r
	defer func() {
		if err != nil {
			opened.Close()
		}
	}()

	r.posts, err = os.Open(filepath.Join(dir, "postings.bin"))
	if err != nil {
		return nil, err
	}
	var postingsHeader [4]byte
	if _, err = io.ReadFull(r.posts, postingsHeader[:]); err != nil {
		return nil, err
	}
	if string(postingsHeader[:]) != "FIP1" {
		return nil, fmt.Errorf("bad postings.bin header")
	}

	r.blocks, err = os.Open(filepath.Join(dir, "blocks.bin"))
	if err != nil {
		return nil, err
	}
	var blocksHeader [4]byte
	if _, err = io.ReadFull(r.blocks, blocksHeader[:]); err != nil {
		return nil, err
	}
	if string(blocksHeader[:]) != "FIB1" {
		return nil, fmt.Errorf("bad blocks.bin header")
	}

	r.pages, err = os.Open(filepath.Join(dir, "pagetable.bin"))
	if err != nil {
		return nil, err
	}

	r.lexicon, err = LoadLexicon(filepath.Join(dir, "lexicon.tsv"))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Close releases the shared index files and returns the first close error
func (r *Reader) Close() error {
	var first error
	for _, f := range []*os.File{r.posts, r.blocks, r.pages} {
		if f != nil {
			if e := f.Close(); first == nil {
				first = e
			}
		}
	}
	return first
}

// BlockDescriptor locates one compressed posting block and describes its contents
type BlockDescriptor struct {
	LastDocID, PostingCount uint32
	PayloadOffset           uint64
	DocumentBytes           uint32
	FrequencyBytes          uint32
}

// BlockDescriptors reads only this term's consecutive descriptors in one read
func (r *Reader) BlockDescriptors(entry Entry) ([]BlockDescriptor, error) {
	if entry.BlocksOffset < 4 || entry.BlocksOffset%24 != 4 ||
		entry.BlocksOffset > math.MaxInt64 || entry.BlockCount == 0 ||
		entry.BlockCount > uint64(math.MaxInt)/24 ||
		entry.BlockCount > (math.MaxInt64-entry.BlocksOffset)/24 {
		return nil, fmt.Errorf("invalid block descriptor range")
	}
	byte_length := entry.BlockCount * 24
	file_info, err := r.blocks.Stat()
	if err != nil {
		return nil, err
	}
	if entry.BlocksOffset+byte_length > uint64(file_info.Size()) {
		return nil, fmt.Errorf("block descriptor range exceeds file size")
	}
	encoded := make([]byte, int(byte_length))
	if _, err := r.blocks.ReadAt(encoded, int64(entry.BlocksOffset)); err != nil {
		return nil, err
	}
	descriptors := make([]BlockDescriptor, int(entry.BlockCount))
	var posting_count uint64
	for index := range descriptors {
		decoded, err := decodeBlockDescriptor(encoded[index*24 : (index+1)*24])
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", index, err)
		}
		if index > 0 && decoded.LastDocID <= descriptors[index-1].LastDocID {
			return nil, fmt.Errorf("block docIDs are not increasing")
		}
		descriptors[index] = decoded
		posting_count += uint64(decoded.PostingCount)
	}
	if posting_count != entry.DocFrequency {
		return nil, fmt.Errorf("block posting count does not match lexicon document frequency")
	}
	return descriptors, nil
}

// decodeBlockDescriptor decodes one descriptor and checks posting counts and chunk lengths
func decodeBlockDescriptor(header []byte) (BlockDescriptor, error) {
	descriptor := BlockDescriptor{
		LastDocID:      binary.LittleEndian.Uint32(header[0:4]),
		PostingCount:   binary.LittleEndian.Uint32(header[4:8]),
		PayloadOffset:  binary.LittleEndian.Uint64(header[8:16]),
		DocumentBytes:  binary.LittleEndian.Uint32(header[16:20]),
		FrequencyBytes: binary.LittleEndian.Uint32(header[20:24]),
	}
	posting_count := uint64(descriptor.PostingCount)
	if posting_count == 0 || posting_count > 65536 ||
		uint64(descriptor.DocumentBytes) < posting_count || uint64(descriptor.DocumentBytes) > 5*posting_count ||
		uint64(descriptor.FrequencyBytes) < posting_count || uint64(descriptor.FrequencyBytes) > 5*posting_count ||
		descriptor.PayloadOffset > math.MaxInt64 {
		return descriptor, fmt.Errorf("invalid block descriptor")
	}
	return descriptor, nil
}

// readDocumentBlock reads only the docID chunk. Frequencies stay on disk
func (r *Reader) readDocumentBlock(descriptor BlockDescriptor) ([]uint32, error) {
	if err := validatePayloadDescriptor(descriptor); err != nil {
		return nil, err
	}
	payload := make([]byte, int(descriptor.DocumentBytes))
	if _, err := r.posts.ReadAt(payload, int64(descriptor.PayloadOffset)); err != nil {
		return nil, err
	}
	encoded := bytes.NewReader(payload)
	docs := make([]uint32, descriptor.PostingCount)
	var doc uint64
	for i := range docs {
		gap, err := binary.ReadUvarint(encoded)
		if err != nil {
			return nil, err
		}
		if gap > math.MaxUint32-doc || i > 0 && gap == 0 {
			return nil, fmt.Errorf("invalid document gap")
		}
		doc += gap
		docs[i] = uint32(doc)
	}
	if doc != uint64(descriptor.LastDocID) || encoded.Len() != 0 {
		return nil, fmt.Errorf("inconsistent document chunk")
	}
	return docs, nil
}

// readFrequencyBlock decodes positive term frequencies for one posting block
func (r *Reader) readFrequencyBlock(descriptor BlockDescriptor) ([]uint32, error) {
	if err := validatePayloadDescriptor(descriptor); err != nil {
		return nil, err
	}
	payload := make([]byte, int(descriptor.FrequencyBytes))
	offset := descriptor.PayloadOffset + uint64(descriptor.DocumentBytes)
	if _, err := r.posts.ReadAt(payload, int64(offset)); err != nil {
		return nil, err
	}
	encoded := bytes.NewReader(payload)
	freqs := make([]uint32, descriptor.PostingCount)
	for i := range freqs {
		freq, err := binary.ReadUvarint(encoded)
		if err != nil {
			return nil, err
		}
		if freq == 0 || freq > math.MaxUint32 {
			return nil, fmt.Errorf("invalid frequency")
		}
		freqs[i] = uint32(freq)
	}
	if encoded.Len() != 0 {
		return nil, fmt.Errorf("inconsistent frequency chunk")
	}
	return freqs, nil
}

// validatePayloadDescriptor checks chunk sizes and offsets before reading posting data
func validatePayloadDescriptor(descriptor BlockDescriptor) error {
	count := uint64(descriptor.PostingCount)
	docs, freqs := uint64(descriptor.DocumentBytes), uint64(descriptor.FrequencyBytes)
	if count == 0 || count > 65536 || docs < count || docs > 5*count ||
		freqs < count || freqs > 5*count || descriptor.PayloadOffset < 4 ||
		descriptor.PayloadOffset > math.MaxInt64-docs-freqs {
		return fmt.Errorf("invalid posting block descriptor")
	}
	return nil
}

// Document is the original Rust 16-byte page-table record
type Document struct {
	PID, Length uint32
	Offset      uint64
}

// Document reads the fixed size page table record for a docID
func (r *Reader) Document(docID uint32) (Document, error) {
	var b [16]byte
	if _, err := r.pages.ReadAt(b[:], int64(docID)*16); err != nil {
		return Document{}, err
	}
	return Document{binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint32(b[4:8]), binary.LittleEndian.Uint64(b[8:16])}, nil
}
