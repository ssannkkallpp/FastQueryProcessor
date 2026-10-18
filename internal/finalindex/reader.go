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

// Entry is one row of lexicon.tsv. Offsets are absolute bytes in blocks.bin.
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

// Reader supports block selection and fixed-position document-table lookup.
// It does not load the lexicon: a query processor can choose its lookup strategy.
type Reader struct{ posts, blocks, pages *os.File }

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
	for _, item := range []struct {
		name, magic string
		dst         **os.File
	}{{"postings.bin", "FIP1", &r.posts}, {"blocks.bin", "FIB1", &r.blocks}, {"pagetable.bin", "", &r.pages}} {
		*item.dst, err = os.Open(filepath.Join(dir, item.name))
		if err != nil {
			return nil, err
		}
		if item.magic != "" {
			var h [4]byte
			if _, err = io.ReadFull(*item.dst, h[:]); err != nil {
				return nil, err
			}
			if string(h[:]) != item.magic {
				return nil, fmt.Errorf("bad %s header", item.name)
			}
		}
	}
	return r, nil
}

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

type block struct {
	last, count uint32
	offset      uint64
	docs, freqs uint32
}

// BlockDescriptor locates one compressed posting block and describes its contents.
type BlockDescriptor struct {
	LastDocID, PostingCount uint32
	PayloadOffset           uint64
	DocumentBytes           uint32
	FrequencyBytes          uint32
}

// BlockDescriptors reads only this term's consecutive descriptors in one read.
// It does not read or decompress any posting payloads.
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
		if index > 0 && decoded.last <= descriptors[index-1].LastDocID {
			return nil, fmt.Errorf("block docIDs are not increasing")
		}
		descriptors[index] = BlockDescriptor{
			LastDocID: decoded.last, PostingCount: decoded.count,
			PayloadOffset: decoded.offset, DocumentBytes: decoded.docs, FrequencyBytes: decoded.freqs,
		}
		posting_count += uint64(decoded.count)
	}
	if posting_count != entry.DocFrequency {
		return nil, fmt.Errorf("block posting count does not match lexicon document frequency")
	}
	return descriptors, nil
}

func (r *Reader) block(e Entry, i uint64) (b block, err error) {
	if e.BlocksOffset > math.MaxInt64 || i >= e.BlockCount || i > (math.MaxInt64-e.BlocksOffset)/24 {
		return b, fmt.Errorf("invalid block index")
	}
	var h [24]byte
	if _, err = r.blocks.ReadAt(h[:], int64(e.BlocksOffset+i*24)); err != nil {
		return b, err
	}
	return decodeBlockDescriptor(h[:])
}

func decodeBlockDescriptor(h []byte) (b block, err error) {
	b = block{binary.LittleEndian.Uint32(h[0:4]), binary.LittleEndian.Uint32(h[4:8]), binary.LittleEndian.Uint64(h[8:16]), binary.LittleEndian.Uint32(h[16:20]), binary.LittleEndian.Uint32(h[20:24])}
	if b.count == 0 || b.count > 65536 || b.docs < b.count || uint64(b.docs) > 5*uint64(b.count) || b.freqs < b.count || uint64(b.freqs) > 5*uint64(b.count) || b.offset > math.MaxInt64 {
		return b, fmt.Errorf("invalid block descriptor")
	}
	return b, nil
}

type Posting struct{ DocID, Frequency uint32 }

// ReadPostingBlock loads and decodes one descriptor's payload from postings.bin.
func (r *Reader) ReadPostingBlock(descriptor BlockDescriptor) ([]Posting, error) {
	count := uint64(descriptor.PostingCount)
	document_bytes, frequency_bytes := uint64(descriptor.DocumentBytes), uint64(descriptor.FrequencyBytes)
	payload_bytes := document_bytes + frequency_bytes
	if count == 0 || count > 65536 || document_bytes < count || document_bytes > 5*count ||
		frequency_bytes < count || frequency_bytes > 5*count || descriptor.PayloadOffset < 4 ||
		descriptor.PayloadOffset > math.MaxInt64-payload_bytes {
		return nil, fmt.Errorf("invalid posting block descriptor")
	}
	payload := make([]byte, int(payload_bytes))
	if _, err := r.posts.ReadAt(payload, int64(descriptor.PayloadOffset)); err != nil {
		return nil, err
	}
	document_reader := bytes.NewReader(payload[:descriptor.DocumentBytes])
	frequency_reader := bytes.NewReader(payload[descriptor.DocumentBytes:])
	postings := make([]Posting, 0, descriptor.PostingCount)
	var doc_id uint64
	for index := uint32(0); index < descriptor.PostingCount; index++ {
		gap, err := binary.ReadUvarint(document_reader)
		if err != nil {
			return nil, err
		}
		frequency, err := binary.ReadUvarint(frequency_reader)
		if err != nil {
			return nil, err
		}
		if gap > math.MaxUint32-doc_id || index > 0 && gap == 0 || frequency == 0 || frequency > math.MaxUint32 {
			return nil, fmt.Errorf("invalid block posting")
		}
		doc_id += gap
		postings = append(postings, Posting{DocID: uint32(doc_id), Frequency: uint32(frequency)})
	}
	if doc_id != uint64(descriptor.LastDocID) || document_reader.Len() != 0 || frequency_reader.Len() != 0 {
		return nil, fmt.Errorf("inconsistent posting block")
	}
	return postings, nil
}

// SeekGE returns the first posting whose docID is >= target, or ok=false.
// It binary-searches block maxima and decodes at most one posting block.
func (r *Reader) SeekGE(e Entry, target uint32) (p Posting, ok bool, err error) {
	lo, hi := uint64(0), e.BlockCount
	for lo < hi {
		mid := lo + (hi-lo)/2
		b, er := r.block(e, mid)
		if er != nil {
			return p, false, er
		}
		if b.last < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == e.BlockCount {
		return p, false, nil
	}
	b, err := r.block(e, lo)
	if err != nil {
		return p, false, err
	}
	postings, err := r.ReadPostingBlock(BlockDescriptor{
		LastDocID: b.last, PostingCount: b.count, PayloadOffset: b.offset,
		DocumentBytes: b.docs, FrequencyBytes: b.freqs,
	})
	if err != nil {
		return p, false, err
	}
	for _, posting := range postings {
		if posting.DocID >= target {
			return posting, true, nil
		}
	}

	return p, ok, nil
}

// Document is the original Rust 16-byte page-table record.
type Document struct {
	PID, Length uint32
	Offset      uint64
}

func (r *Reader) Document(docID uint32) (Document, error) {
	var b [16]byte
	if _, err := r.pages.ReadAt(b[:], int64(docID)*16); err != nil {
		return Document{}, err
	}
	return Document{binary.LittleEndian.Uint32(b[0:4]), binary.LittleEndian.Uint32(b[4:8]), binary.LittleEndian.Uint64(b[8:16])}, nil
}
