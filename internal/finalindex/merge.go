// Package finalindex merges Rust IRN1 runs and reads a block-compressed index
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

// runReader reads one sorted input run and holds its current term and compressed postings
type runReader struct {
	file           *os.File
	reader         *bufio.Reader
	remaining      uint64 // Excludes currently processed term
	term, previous string
	count, last    uint64
	docs, freqs    []byte
	order          int
}

// indexOutput manages the final index files, buffered writers, and byte offsets
type indexOutput struct {
	files                                          []*os.File
	postings_writer, blocks_writer, lexicon_writer *bufio.Writer
	postings_offset, blocks_offset                 uint64
	incomplete_marker                              string
}

// runDocumentRange tracks the smallest and largest docIDs seen in one input run
type runDocumentRange struct {
	min_doc_id, max_doc_id uint64
	has_postings           bool
}

// runHeap selects the reader with the smallest current term, breaking ties by run number
type runHeap []*runReader

// Stats counts the distinct terms, postings, and blocks written by the merger
type Stats struct{ Terms, Postings, Blocks uint64 }

// termBlocks builds consecutive output blocks for one term across all contributing runs
type termBlocks struct {
	output                              *indexOutput
	block_size                          int
	descriptor_start                    uint64
	document_frequency, block_count     uint64
	document_bytes, frequency_bytes     []byte
	posting_count                       uint32
	last_block_doc_id, last_term_doc_id uint64
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
		// Validate the end of the file
		_, e := r.reader.Peek(1)
		if e == io.EOF {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		return false, fmt.Errorf("trailing run bytes")
	}

	term_length, e := binary.ReadUvarint(r.reader)
	if e != nil {
		return false, e
	}
	if term_length == 0 || term_length > 64 {
		return false, fmt.Errorf("invalid term length %d", term_length)
	}

	t := make([]byte, int(term_length))
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
	// A u32 varint occupies 1..5 bytes. Bound allocations using the input size too
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

// validate_metadata checks configuration and parser outputs before merging
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

// Merge validates the inputs, merges one complete term at a time, and finalizes
// a new output directory. Input runs and the on-disk formats remain unchanged
func Merge(input_path, output_path string, blockSize int) (stats Stats, err error) {
	collection_metadata, document_count, expected_postings, run_paths, err := validate_metadata(input_path, blockSize)
	if err != nil {
		return stats, err
	}

	// Open every run produced by the Rust executable and push them all to the heap
	run_readers, term_heap, err := openRunReaders(run_paths)
	if err != nil {
		return stats, err
	}
	defer closeRunReaders(run_readers)

	// Create the output files for the phase 2 merge
	output, err := createIndexOutput(output_path)
	if err != nil {
		return stats, err
	}
	defer output.close()

	// Track each input run's minimum and maximum docIDs to detect/avoid overlapping document ranges
	run_ranges := make([]runDocumentRange, len(run_readers))

	// Each call consumes this term from every matching run before starting another
	for term_heap.Len() > 0 {
		if err := mergeNextTerm(&term_heap, output, blockSize, document_count, run_ranges, &stats); err != nil {
			return stats, err
		}
	}
	if err := validateRunRanges(run_ranges); err != nil {
		return stats, err
	}
	if stats.Postings != expected_postings {
		return stats, fmt.Errorf("merged %d postings, metadata expects %d", stats.Postings, expected_postings)
	}
	if err := output.finish(input_path, output_path, collection_metadata, document_count, blockSize, stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// openRunReaders loads one term record per run and puts nonempty runs in the heap
func openRunReaders(run_paths []string) ([]*runReader, runHeap, error) {
	run_readers := make([]*runReader, 0, len(run_paths))
	var term_heap runHeap
	for run_number, run_path := range run_paths {
		run_reader, err := openRun(run_path, run_number)
		if err != nil {
			// Fail fully on any failures
			closeRunReaders(run_readers)
			return nil, nil, err
		}
		run_readers = append(run_readers, run_reader)
		has_record, err := run_reader.next()
		if err != nil {
			closeRunReaders(run_readers)
			return nil, nil, fmt.Errorf("%s: %w", run_path, err)
		}
		if has_record {
			heap.Push(&term_heap, run_reader)
		}
	}
	return run_readers, term_heap, nil
}

func closeRunReaders(run_readers []*runReader) {
	for _, run_reader := range run_readers {
		run_reader.file.Close()
	}
}

// createIndexOutput creates the output directory, files, and format headers
func createIndexOutput(output_path string) (*indexOutput, error) {
	if err := os.Mkdir(output_path, 0755); err != nil {
		return nil, err
	}
	output := &indexOutput{
		postings_offset: 4, blocks_offset: 4,
		incomplete_marker: filepath.Join(output_path, "INCOMPLETE"),
	}
	if err := os.WriteFile(output.incomplete_marker, []byte("merge in progress\n"), 0644); err != nil {
		return nil, err
	}
	specifications := []struct {
		name     string
		capacity int
		writer   **bufio.Writer
	}{
		{"postings.bin", 4 << 20, &output.postings_writer},
		{"blocks.bin", 1 << 20, &output.blocks_writer},
		{"lexicon.tsv", 1 << 20, &output.lexicon_writer},
	}
	for _, specification := range specifications {
		file, err := os.Create(filepath.Join(output_path, specification.name))
		if err != nil {
			output.close()
			return nil, err
		}
		output.files = append(output.files, file)
		*specification.writer = bufio.NewWriterSize(file, specification.capacity)
	}
	if err := output.writeHeaders(); err != nil {
		output.close()
		return nil, err
	}
	return output, nil
}

func (output *indexOutput) writeHeaders() error {
	if _, err := output.postings_writer.WriteString("FIP1"); err != nil {
		return err
	}
	if _, err := output.blocks_writer.WriteString("FIB1"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output.lexicon_writer, "term\tdoc_frequency\tblocks_offset\tblock_count")
	return err
}

// close releases all remaining files, even if closing one of them fails
func (output *indexOutput) close() error {
	var first_error error
	for _, file := range output.files {
		if err := file.Close(); err != nil && first_error == nil {
			first_error = err
		}
	}
	output.files = nil
	return first_error
}

func newTermBlocks(output *indexOutput, block_size int) *termBlocks {
	return &termBlocks{
		output: output, block_size: block_size, descriptor_start: output.blocks_offset,
		document_bytes:  make([]byte, 0, block_size*3),
		frequency_bytes: make([]byte, 0, block_size*2),
	}
}

// appendPosting encodes one posting and writes a block when it reaches capacity
func (term_blocks *termBlocks) appendPosting(doc_id, frequency uint64, stats *Stats) error {
	document_gap := doc_id // Every block restarts its gaps from zero
	if term_blocks.posting_count > 0 {
		document_gap = doc_id - term_blocks.last_block_doc_id
	}
	term_blocks.document_bytes = binary.AppendUvarint(term_blocks.document_bytes, document_gap)
	term_blocks.frequency_bytes = binary.AppendUvarint(term_blocks.frequency_bytes, frequency)
	term_blocks.posting_count++
	term_blocks.last_block_doc_id = doc_id
	term_blocks.last_term_doc_id = doc_id
	term_blocks.document_frequency++
	stats.Postings++
	if int(term_blocks.posting_count) == term_blocks.block_size {
		return term_blocks.flushBlock(stats)
	}
	return nil
}

func (term_blocks *termBlocks) flushBlock(stats *Stats) error {
	if term_blocks.posting_count == 0 {
		return nil
	}

	output := term_blocks.output
	var descriptor [24]byte
	binary.LittleEndian.PutUint32(descriptor[0:4], uint32(term_blocks.last_block_doc_id))
	binary.LittleEndian.PutUint32(descriptor[4:8], term_blocks.posting_count)
	binary.LittleEndian.PutUint64(descriptor[8:16], output.postings_offset)
	binary.LittleEndian.PutUint32(descriptor[16:20], uint32(len(term_blocks.document_bytes)))
	binary.LittleEndian.PutUint32(descriptor[20:24], uint32(len(term_blocks.frequency_bytes)))
	if _, err := output.blocks_writer.Write(descriptor[:]); err != nil {
		return err
	}
	if _, err := output.postings_writer.Write(term_blocks.document_bytes); err != nil {
		return err
	}
	if _, err := output.postings_writer.Write(term_blocks.frequency_bytes); err != nil {
		return err
	}

	output.postings_offset += uint64(len(term_blocks.document_bytes) + len(term_blocks.frequency_bytes))
	output.blocks_offset += 24
	term_blocks.block_count++
	stats.Blocks++
	// Clear out term_blocks struct for future blocks
	term_blocks.posting_count = 0
	term_blocks.document_bytes = term_blocks.document_bytes[:0]
	term_blocks.frequency_bytes = term_blocks.frequency_bytes[:0]
	return nil
}

func (term_blocks *termBlocks) finishTerm(term string, stats *Stats) error {
	if err := term_blocks.flushBlock(stats); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(term_blocks.output.lexicon_writer, "%s\t%d\t%d\t%d\n",
		term, term_blocks.document_frequency, term_blocks.descriptor_start, term_blocks.block_count); err != nil {
		return err
	}
	stats.Terms++
	return nil
}

// mergeNextTerm gathers all records for the smallest term into consecutive blocks
func mergeNextTerm(term_heap *runHeap, output *indexOutput, block_size int, document_count uint64, run_ranges []runDocumentRange, stats *Stats) error {
	term := (*term_heap)[0].term
	term_blocks := newTermBlocks(output, block_size)
	for term_heap.Len() > 0 && (*term_heap)[0].term == term {
		run_reader := heap.Pop(term_heap).(*runReader)
		if err := appendRunRecord(run_reader, term_blocks, document_count, &run_ranges[run_reader.order], stats); err != nil {
			return err
		}
		// Mutate the reader's current term while it is outside the heap
		has_record, err := run_reader.next()
		if err != nil {
			return err
		}
		// If the run still has more terms, push those to the heap
		if has_record {
			heap.Push(term_heap, run_reader)
		}
	}
	return term_blocks.finishTerm(term, stats)
}

// appendRunRecord decodes one input list, validates it, and feeds output blocks
func appendRunRecord(run_reader *runReader, term_blocks *termBlocks, document_count uint64, run_range *runDocumentRange, stats *Stats) error {
	document_reader := bytes.NewReader(run_reader.docs)
	frequency_reader := bytes.NewReader(run_reader.freqs)
	var doc_id uint64 // Starts at 0
	for posting_number := uint64(0); posting_number < run_reader.count; posting_number++ {
		document_gap, err := binary.ReadUvarint(document_reader)
		if err != nil {
			return fmt.Errorf("%s doc gap: %w", run_reader.term, err)
		}
		frequency, err := binary.ReadUvarint(frequency_reader)
		if err != nil {
			return err
		}
		if document_gap > math.MaxUint32-doc_id || posting_number > 0 && document_gap == 0 || frequency == 0 || frequency > math.MaxUint32 {
			return fmt.Errorf("%s: invalid gap or frequency", run_reader.term)
		}
		doc_id += document_gap
		if doc_id >= document_count || term_blocks.document_frequency > 0 && doc_id <= term_blocks.last_term_doc_id {
			return fmt.Errorf("%s: invalid or overlapping docID %d", run_reader.term, doc_id)
		}
		// Update current run's min/max docid
		run_range.record(doc_id)
		if err := term_blocks.appendPosting(doc_id, frequency, stats); err != nil {
			return err
		}
	}
	if doc_id != run_reader.last || document_reader.Len() != 0 || frequency_reader.Len() != 0 {
		return fmt.Errorf("%s: run count, last_doc or block lengths inconsistent", run_reader.term)
	}
	return nil
}

// record updates the range with a docID from any term in this run
func (run_range *runDocumentRange) record(doc_id uint64) {
	if !run_range.has_postings {
		run_range.min_doc_id, run_range.max_doc_id = doc_id, doc_id
		run_range.has_postings = true
		return
	}
	if doc_id < run_range.min_doc_id {
		run_range.min_doc_id = doc_id
	}
	if doc_id > run_range.max_doc_id {
		run_range.max_doc_id = doc_id
	}
}

func validateRunRanges(run_ranges []runDocumentRange) error {
	var previous_max_doc_id uint64
	previous_run_has_postings := false
	for run_number, run_range := range run_ranges {
		if !run_range.has_postings {
			continue
		}
		if previous_run_has_postings && run_range.min_doc_id <= previous_max_doc_id {
			return fmt.Errorf("run %d overlaps earlier run document ranges", run_number)
		}
		previous_max_doc_id = run_range.max_doc_id
		previous_run_has_postings = true
	}
	return nil
}

// finish flushes and closes the index before publishing its metadata and marker
func (output *indexOutput) finish(input_path, output_path string, collection_metadata []byte, document_count uint64, block_size int, stats Stats) error {
	for _, writer := range []*bufio.Writer{output.postings_writer, output.blocks_writer, output.lexicon_writer} {
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	if err := output.close(); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(input_path, "pagetable.bin"), filepath.Join(output_path, "pagetable.bin")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output_path, "collection-meta.txt"), collection_metadata, 0644); err != nil {
		return err
	}
	final_metadata := fmt.Sprintf("format=FIP1\nnum_docs=%d\nnum_terms=%d\nnum_postings=%d\nnum_blocks=%d\nblock_size=%d\n",
		document_count, stats.Terms, stats.Postings, stats.Blocks, block_size)
	if err := os.WriteFile(filepath.Join(output_path, "meta.txt"), []byte(final_metadata), 0644); err != nil {
		return err
	}
	return os.Remove(output.incomplete_marker)
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
