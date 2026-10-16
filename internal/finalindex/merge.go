// Package finalindex merges Rust IRN1 runs and reads a block-compressed index.
package finalindex

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type runReader struct {
	file           *os.File
	reader         *bufio.Reader
	remaining      uint64 // Excludes currently processed term
	term, previous string
	count, last    uint64
	docs, freqs    []byte
	order          int
}

func openRun(filepath string, order int) (*runReader, error) {
	file, e := os.Open(filepath)
	if e != nil {
		return nil, e
	}

	r := &runReader{file: file, reader: bufio.NewReaderSize(file, 1<<20), order: order}
	var h [12]byte
	if _, e = io.ReadFull(r.reader, h[:]); e != nil {
		file.Close()
		return nil, e
	}

	if string(h[:4]) != "IRN1" {
		file.Close()
		return nil, fmt.Errorf("%s: expected IRN1 binary run", filepath)
	}
	r.remaining = binary.LittleEndian.Uint64(h[4:])
	return r, nil
}

func (r *runReader) next() (bool, error) {
	if r.remaining == 0 {
		_, e := r.reader.Peek(1)
		if e == io.EOF {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		return false, fmt.Errorf("trailing run bytes")
	}
	n, e := binary.ReadUvarint(r.reader)
	if e != nil {
		return false, e
	}
	if n == 0 || n > 64 {
		return false, fmt.Errorf("invalid term length %d", n)
	}
	t := make([]byte, int(n))
	if _, e = io.ReadFull(r.reader, t); e != nil {
		return false, e
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false, fmt.Errorf("invalid term bytes")
		}
	}
	r.term = string(t)
	if r.previous != "" && r.term <= r.previous {
		return false, fmt.Errorf("terms out of order")
	}
	r.previous = r.term
	values := [4]uint64{}
	for i := range values {
		values[i], e = binary.ReadUvarint(r.reader)
		if e != nil {
			return false, e
		}
	}
	r.count, r.last = values[0], values[1]
	if r.count == 0 || r.count > math.MaxUint32 || r.last > math.MaxUint32 {
		return false, fmt.Errorf("invalid posting count or last docID")
	}
	// A u32 varint occupies 1..5 bytes. Bound allocations using the input size too.
	st, e := r.file.Stat()
	if e != nil {
		return false, e
	}
	for _, n := range values[2:] {
		if n < r.count || n > 5*r.count || n > uint64(st.Size()) {
			return false, fmt.Errorf("invalid posting block length")
		}
	}
	r.docs = resize(r.docs, int(values[2]))
	r.freqs = resize(r.freqs, int(values[3]))
	if _, e = io.ReadFull(r.reader, r.docs); e != nil {
		return false, e
	}
	if _, e = io.ReadFull(r.reader, r.freqs); e != nil {
		return false, e
	}
	r.remaining--
	return true, nil
}

// Resize the slice to size n
func resize(b []byte, n int) []byte {
	if cap(b) < n {
		return make([]byte, n)
	}
	return b[:n]
}

type runHeap []*runReader

func (h runHeap) Len() int { return len(h) }
func (h runHeap) Less(i, j int) bool {
	if h[i].term == h[j].term {
		return h[i].order < h[j].order
	}
	return h[i].term < h[j].term
}
func (h runHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(v any)   { *h = append(*h, v.(*runReader)) }
func (h *runHeap) Pop() any     { a := *h; v := a[len(a)-1]; *h = a[:len(a)-1]; return v }

// Stats describes a completed merge.
type Stats struct{ Terms, Postings, Blocks uint64 }

// validate_metadata checks configuration and parser outputs before merging.
func validate_metadata(input_path string, blockSize int) (meta []byte, numDocs, expected uint64, paths []string, err error) {
	if blockSize < 1 || blockSize > 65536 {
		return nil, 0, 0, nil, fmt.Errorf("block size must be 1..65536")
	}
	meta, e := os.ReadFile(filepath.Join(input_path, "meta.txt"))
	if e != nil {
		return nil, 0, 0, nil, e
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(meta), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			fields[k] = v
		}
	}
	numDocs, e = strconv.ParseUint(fields["num_docs"], 10, 32)
	if e != nil {
		return nil, 0, 0, nil, fmt.Errorf("invalid num_docs: %w", e)
	}
	expected, e = strconv.ParseUint(fields["num_postings"], 10, 64)
	if e != nil {
		return nil, 0, 0, nil, fmt.Errorf("invalid num_postings: %w", e)
	}
	pages, e := os.Stat(filepath.Join(input_path, "pagetable.bin"))
	if e != nil {
		return nil, 0, 0, nil, e
	}
	if uint64(pages.Size()) != numDocs*16 {
		return nil, 0, 0, nil, fmt.Errorf("page table size does not match num_docs")
	}
	paths, e = filepath.Glob(filepath.Join(input_path, "runs", "postings-*.bin"))
	if e != nil {
		return nil, 0, 0, nil, e
	}
	sort.Strings(paths)
	nr, e := strconv.Atoi(fields["num_runs"])
	if e != nil || nr != len(paths) {
		return nil, 0, 0, nil, fmt.Errorf("run count does not match metadata")
	}
	if len(paths) == 0 && expected != 0 {
		return nil, 0, 0, nil, fmt.Errorf("no intermediate binaries found")
	}
	return meta, numDocs, expected, paths, nil
}

// Merge creates a new output directory final-index with the final compressed index
// Input must contain runs/, pagetable.bin and meta.txt from the Rust parser.
func Merge(input_path, output_path string, blockSize int) (stats Stats, err error) {
	meta, numDocs, expected, paths, e := validate_metadata(input_path, blockSize)
	if e != nil {
		return stats, e
	}
	readers := make([]*runReader, 0, len(paths))
	defer func() {
		for _, r := range readers {
			r.file.Close()
		}
	}()
	var h runHeap
	for i, p := range paths {
		r, e := openRun(p, i)
		if e != nil {
			return stats, e
		}
		readers = append(readers, r)
		ok, e := r.next()
		if e != nil {
			return stats, fmt.Errorf("%s: %w", p, e)
		}
		if ok {
			heap.Push(&h, r)
		}
	}
	if e = os.Mkdir(output_path, 0755); e != nil {
		return stats, e
	}
	// A failed build remains marked incomplete, never as a completed index.
	marker := filepath.Join(output_path, "INCOMPLETE")
	if e = os.WriteFile(marker, []byte("merge in progress\n"), 0644); e != nil {
		return stats, e
	}
	files := make([]*os.File, 0, 3)
	defer func() {
		for _, f := range files {
			if e := f.Close(); err == nil {
				err = e
			}
		}
	}()
	create := func(name string) (*os.File, error) {
		f, e := os.Create(filepath.Join(output_path, name))
		if e == nil {
			files = append(files, f)
		}
		return f, e
	}
	pf, e := create("postings.bin")
	if e != nil {
		return stats, e
	}
	bf, e := create("blocks.bin")
	if e != nil {
		return stats, e
	}
	lf, e := create("lexicon.tsv")
	if e != nil {
		return stats, e
	}
	pw, bw, lw := bufio.NewWriterSize(pf, 4<<20), bufio.NewWriterSize(bf, 1<<20), bufio.NewWriterSize(lf, 1<<20)
	if _, e = pw.WriteString("FIP1"); e != nil {
		return stats, e
	}
	if _, e = bw.WriteString("FIB1"); e != nil {
		return stats, e
	}
	if _, e = fmt.Fprintln(lw, "term\tdoc_frequency\tblocks_offset\tblock_count"); e != nil {
		return stats, e
	}
	postOffset, blockOffset := uint64(4), uint64(4)
	// Run doc ranges must not overlap, including for terms absent from a run.
	mins, maxs := make([]uint64, len(readers)), make([]uint64, len(readers))
	seen := make([]bool, len(readers))
	for len(h) > 0 {
		term := h[0].term
		termStart := blockOffset
		var df, nb, last uint64
		haveLast := false
		docs, freqs := make([]byte, 0, blockSize*3), make([]byte, 0, blockSize*2)
		count := uint32(0)
		var blockLast uint64
		flush := func() error {
			if count == 0 {
				return nil
			}
			var header [24]byte
			binary.LittleEndian.PutUint32(header[0:4], uint32(blockLast))
			binary.LittleEndian.PutUint32(header[4:8], count)
			binary.LittleEndian.PutUint64(header[8:16], postOffset)
			binary.LittleEndian.PutUint32(header[16:20], uint32(len(docs)))
			binary.LittleEndian.PutUint32(header[20:24], uint32(len(freqs)))
			if _, e := bw.Write(header[:]); e != nil {
				return e
			}
			if _, e := pw.Write(docs); e != nil {
				return e
			}
			if _, e := pw.Write(freqs); e != nil {
				return e
			}
			postOffset += uint64(len(docs) + len(freqs))
			blockOffset += 24
			nb++
			stats.Blocks++
			count = 0
			docs = docs[:0]
			freqs = freqs[:0]
			return nil
		}
		for len(h) > 0 && h[0].term == term {
			r := heap.Pop(&h).(*runReader)
			dr, fr := bytes.NewReader(r.docs), bytes.NewReader(r.freqs)
			var doc uint64
			for i := uint64(0); i < r.count; i++ {
				gap, e := binary.ReadUvarint(dr)
				if e != nil {
					return stats, fmt.Errorf("%s doc gap: %w", term, e)
				}
				freq, e := binary.ReadUvarint(fr)
				if e != nil {
					return stats, e
				}
				if gap > math.MaxUint32-doc || i > 0 && gap == 0 || freq == 0 || freq > math.MaxUint32 {
					return stats, fmt.Errorf("%s: invalid gap or frequency", term)
				}
				doc += gap
				if doc >= numDocs || haveLast && doc <= last {
					return stats, fmt.Errorf("%s: invalid or overlapping docID %d", term, doc)
				}
				if !seen[r.order] {
					mins[r.order] = doc
					maxs[r.order] = doc
					seen[r.order] = true
				} else {
					if doc < mins[r.order] {
						mins[r.order] = doc
					}
					if doc > maxs[r.order] {
						maxs[r.order] = doc
					}
				}
				encodedGap := doc
				if count > 0 {
					encodedGap = doc - blockLast
				}
				docs = binary.AppendUvarint(docs, encodedGap)
				freqs = binary.AppendUvarint(freqs, freq)
				count++
				blockLast = doc
				last = doc
				haveLast = true
				df++
				stats.Postings++
				if int(count) == blockSize {
					if e = flush(); e != nil {
						return stats, e
					}
				}
			}
			if doc != r.last || dr.Len() != 0 || fr.Len() != 0 {
				return stats, fmt.Errorf("%s: run count, last_doc or block lengths inconsistent", term)
			}
			ok, e := r.next()
			if e != nil {
				return stats, e
			}
			if ok {
				heap.Push(&h, r)
			}
		}
		if e = flush(); e != nil {
			return stats, e
		}
		if _, e = fmt.Fprintf(lw, "%s\t%d\t%d\t%d\n", term, df, termStart, nb); e != nil {
			return stats, e
		}
		stats.Terms++
	}
	var previousMax uint64
	previousSeen := false
	for i := range seen {
		if seen[i] {
			if previousSeen && mins[i] <= previousMax {
				return stats, fmt.Errorf("run %d overlaps earlier run document ranges", i)
			}
			previousMax = maxs[i]
			previousSeen = true
		}
	}
	if stats.Postings != expected {
		return stats, fmt.Errorf("merged %d postings, metadata expects %d", stats.Postings, expected)
	}
	for _, w := range []*bufio.Writer{pw, bw, lw} {
		if e = w.Flush(); e != nil {
			return stats, e
		}
	}
	// Close and check all output files before marking the index complete.
	for _, f := range files {
		if e = f.Close(); e != nil {
			files = nil
			return stats, e
		}
	}
	files = nil
	if e = copyFile(filepath.Join(input_path, "pagetable.bin"), filepath.Join(output_path, "pagetable.bin")); e != nil {
		return stats, e
	}
	if e = os.WriteFile(filepath.Join(output_path, "collection-meta.txt"), meta, 0644); e != nil {
		return stats, e
	}
	finalMeta := fmt.Sprintf("format=FIP1\nnum_docs=%d\nnum_terms=%d\nnum_postings=%d\nnum_blocks=%d\nblock_size=%d\n", numDocs, stats.Terms, stats.Postings, stats.Blocks, blockSize)
	if e = os.WriteFile(filepath.Join(output_path, "meta.txt"), []byte(finalMeta), 0644); e != nil {
		return stats, e
	}
	err = os.Remove(marker)
	return stats, err
}

func copyFile(src, dst string) (err error) {
	in, e := os.Open(src)
	if e != nil {
		return e
	}

	defer in.Close()

	out, e := os.Create(dst)
	if e != nil {
		return e
	}

	defer func() {
		if e := out.Close(); err == nil {
			err = e
		}
	}()

	_, err = io.Copy(out, in)
	return err
}
