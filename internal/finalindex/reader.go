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

func ParseEntry(line string) (Entry, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 4 {
		return Entry{}, fmt.Errorf("expected four lexicon columns")
	}
	e := Entry{Term: fields[0]}
	var err error
	dst := []*uint64{&e.DocFrequency, &e.BlocksOffset, &e.BlockCount}
	for i, p := range dst {
		*p, err = strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return Entry{}, err
		}
	}
	if e.DocFrequency == 0 || e.BlockCount == 0 || e.BlocksOffset < 4 || e.BlocksOffset%24 != 4 {
		return Entry{}, fmt.Errorf("invalid lexicon entry")
	}
	return e, nil
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

func (r *Reader) block(e Entry, i uint64) (b block, err error) {
	if e.BlocksOffset > math.MaxInt64 || i >= e.BlockCount || i > (math.MaxInt64-e.BlocksOffset)/24 {
		return b, fmt.Errorf("invalid block index")
	}
	var h [24]byte
	if _, err = r.blocks.ReadAt(h[:], int64(e.BlocksOffset+i*24)); err != nil {
		return b, err
	}
	b = block{binary.LittleEndian.Uint32(h[0:4]), binary.LittleEndian.Uint32(h[4:8]), binary.LittleEndian.Uint64(h[8:16]), binary.LittleEndian.Uint32(h[16:20]), binary.LittleEndian.Uint32(h[20:24])}
	if b.count == 0 || b.count > 65536 || b.docs < b.count || uint64(b.docs) > 5*uint64(b.count) || b.freqs < b.count || uint64(b.freqs) > 5*uint64(b.count) || b.offset > math.MaxInt64 {
		return b, fmt.Errorf("invalid block descriptor")
	}
	return b, nil
}

type Posting struct{ DocID, Frequency uint32 }

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
	payload := make([]byte, int(b.docs)+int(b.freqs))
	if _, err = r.posts.ReadAt(payload, int64(b.offset)); err != nil {
		return p, false, err
	}
	dr, fr := bytes.NewReader(payload[:b.docs]), bytes.NewReader(payload[b.docs:])
	var doc uint64
	for i := uint32(0); i < b.count; i++ {
		gap, er := binary.ReadUvarint(dr)
		if er != nil {
			return p, false, er
		}
		freq, er := binary.ReadUvarint(fr)
		if er != nil {
			return p, false, er
		}
		if gap > math.MaxUint32-doc || i > 0 && gap == 0 || freq == 0 || freq > math.MaxUint32 {
			return p, false, fmt.Errorf("invalid block posting")
		}
		doc += gap
		if !ok && doc >= uint64(target) {
			p = Posting{uint32(doc), uint32(freq)}
			ok = true
		}
	}
	if doc != uint64(b.last) || dr.Len() != 0 || fr.Len() != 0 {
		return p, false, fmt.Errorf("inconsistent posting block")
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
