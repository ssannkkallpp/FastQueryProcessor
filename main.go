// main.go: parse unpacked MS MARCO passage collection.
//
// Usage:
//
//	go run main.go -limit 1000 -show
//	go run main.go -input data/collection.tsv
//
// Output: pagetable.tsv in -dir (default ./data), containing
// docID <TAB> pid <TAB> length_in_terms.
// Term occurrences are buffered and written to postings-NNNNNN.tsv as term <TAB> docID.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"sync"
	"time"
)

// Terms longer than this are dropped - Need to edit
const maxTermLen = 64

const postingBufferSize = 64 << 20 // 64 MiB per sorted run, plus sorting overhead.

var postingBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// writeSortedChunk sorts one batch by term, then by numeric docID.
// Each batch is an independent sorted run.
func writeSortedChunk(buffer *bytes.Buffer, out io.Writer) error {
	if buffer.Len() == 0 {
		return nil
	}
	// Each line references the buffer's bytes, which stay intact until writing ends.
	lines := bytes.Split(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), []byte{'\n'})
	sort.Slice(lines, func(i, j int) bool {
		left, right := lines[i], lines[j]
		leftTab, rightTab := bytes.IndexByte(left, '\t'), bytes.IndexByte(right, '\t')
		if order := bytes.Compare(left[:leftTab], right[:rightTab]); order != 0 {
			return order < 0
		}

		// To-do: Could convert these to Ids to integer and compare
		// Then code looks simpler but requires a type conversion at every comparison
		// So we use length for an integer comparison since no leading zeros are possible
		leftID, rightID := left[leftTab+1:], right[rightTab+1:]
		if len(leftID) != len(rightID) {
			return len(leftID) < len(rightID)
		}
		return bytes.Compare(leftID, rightID) < 0
	})

	w := bufio.NewWriter(out)
	for _, line := range lines {
		if _, err := w.Write(line); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	buffer.Reset()
	return nil
}

func writePostings(input, pageTablePath, postingsDir string, limit int, show bool) (docs, tokens uint64, err error) {
	buffer := postingBufferPool.Get().(*bytes.Buffer)
	buffer.Reset()
	defer func() {
		buffer.Reset()
		postingBufferPool.Put(buffer)
	}()

	var writeErr error
	run := 0
	flush := func() {
		if writeErr != nil || buffer.Len() == 0 {
			return
		}
		path := filepath.Join(postingsDir, fmt.Sprintf("postings-%06d.tsv", run))
		out, createErr := os.Create(path)
		if createErr != nil {
			writeErr = createErr
			return
		}
		writeErr = writeSortedChunk(buffer, out)
		if closeErr := out.Close(); writeErr == nil {
			writeErr = closeErr
		}
		run++
	}
	emit := func(term []byte, docID uint32) {
		if writeErr != nil {
			return
		}
		// Write copies the term bytes before parse reuses its term slice.
		buffer.Write(term)
		fmt.Fprintf(buffer, "\t%d\n", docID)
		if show {
			fmt.Printf("%-64s %d\n", term, docID)
		}
		if buffer.Len() >= postingBufferSize {
			flush()
		}
	}

	docs, tokens, err = parse(input, pageTablePath, limit, emit)
	flush() // Include the final batch, even when it is smaller than the threshold.
	if writeErr != nil {
		return docs, tokens, fmt.Errorf("write postings: %w", writeErr)
	}
	return docs, tokens, err
}

// Emit receives one posting per term occurrence. The term slice is reused
// between calls
type Emit func(term []byte, docID uint32)

// parse reads "pid<TAB>text" lines, assigns docIDs in parse order starting
// at 0, tokenizes, calls emit for every term occurrence, and writes the page
// table. limit > 0 stops after that many passages.
//
// Tokenization: lowercase ASCII letters and digits form terms; everything
// else separates terms. A token containing any non-ASCII byte is dropped
// whole (so "cafe" in other scripts doesn't leave junk fragments), which
// satisfies "may just ignore such text" without ever failing on bad bytes.
func parse(tsvPath, pageTablePath string, limit int, emit Emit) (docs, tokens uint64, err error) {
	in, err := os.Open(tsvPath)
	if err != nil {
		return 0, 0, err
	}
	defer in.Close()

	pt, err := os.Create(pageTablePath)
	if err != nil {
		return 0, 0, err
	}
	defer pt.Close()
	ptw := bufio.NewWriterSize(pt, 1<<20)

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20) // logic for handling longer lines, can be revisited later
	term := make([]byte, 0, maxTermLen)

	for sc.Scan() {
		line := sc.Bytes()
		tab := bytes.IndexByte(line, '\t')
		if tab < 0 {
			continue // malformed lines
		}

		pid, text := line[:tab], line[tab+1:]
		docID := uint32(docs)
		var length uint32

		term = term[:0]
		bad_term := false
		for i := 0; i <= len(text); i++ {
			c := byte(' ') // sentinel separator after the last byte flushes the final term
			if i < len(text) {
				c = text[i]
			}
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
				if len(term) >= maxTermLen {
					bad_term = true
				} else {
					term = append(term, c)
				}
			case c >= 'A' && c <= 'Z':
				if len(term) >= maxTermLen {
					bad_term = true
				} else {
					term = append(term, c+('a'-'A'))
				}
			case c >= 0x80:
				bad_term = true
			default: // separator
				if len(term) > 0 && !bad_term {
					emit(term, docID)
					length++
				}
				term = term[:0]
				bad_term = false
			}
		}

		fmt.Fprintf(ptw, "%d\t%s\t%d\n", docID, pid, length)
		docs++
		tokens += uint64(length)
		if limit > 0 && docs >= uint64(limit) {
			break
		}
		if docs%1000000 == 0 {
			fmt.Printf("  parsed %d passages\n", docs)
		}
	}
	if err := sc.Err(); err != nil {
		return docs, tokens, err
	}
	return docs, tokens, ptw.Flush()
}

func main() {
	input := flag.String("input", "data/collection.tsv", "unpacked passage collection")
	dir := flag.String("dir", "data", "output directory for pagetable.tsv and postings-NNNNNN.tsv runs")
	limit := flag.Int("limit", 0, "parse only the first N passages (0 = all)")
	show := flag.Bool("show", false, "print every posting (use with a small -limit)")
	cpuProfile := flag.String("cpuprofile", "", "write CPU profile to this file")
	memProfile := flag.String("memprofile", "", "write heap/allocation profile to this file after parsing")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	var cpuFile *os.File
	if *cpuProfile != "" {
		var err error
		cpuFile, err = os.Create(*cpuProfile)
		if err != nil {
			log.Fatal("create CPU profile: ", err)
		}
		if err := pprof.StartCPUProfile(cpuFile); err != nil {
			cpuFile.Close()
			log.Fatal("start CPU profile: ", err)
		}
	}
	fmt.Println("parsing", *input)
	start := time.Now()

	// Original emit callback and direct parse call, kept for reference - will delete later
	// emit := func(term []byte, docID uint32) {
	// 	if *show {
	// 		fmt.Printf("%-64s %d\n", term, docID)
	// 	}
	// }
	// docs, tokens, err := parse(*input, filepath.Join(*dir, "pagetable.tsv"), *limit, emit)

	docs, tokens, err := writePostings(*input, filepath.Join(*dir, "pagetable.tsv"), *dir, *limit, *show)
	elapsed := time.Since(start)
	if cpuFile != nil {
		pprof.StopCPUProfile()
		if closeErr := cpuFile.Close(); closeErr != nil {
			log.Fatal("close CPU profile: ", closeErr)
		}
	}
	if *memProfile != "" {
		f, profileErr := os.Create(*memProfile)
		if profileErr != nil {
			log.Fatal("create memory profile: ", profileErr)
		}
		runtime.GC() // Report live heap after GC, plus sampled cumulative allocations.
		profileErr = pprof.WriteHeapProfile(f)
		closeErr := f.Close()
		if profileErr != nil {
			log.Fatal("write memory profile: ", profileErr)
		}
		if closeErr != nil {
			log.Fatal("close memory profile: ", closeErr)
		}
	}
	if err != nil {
		log.Fatal("parse: ", err)
	}
	avg := 0.0
	if docs > 0 {
		avg = float64(tokens) / float64(docs)
	}
	fmt.Printf("done: %d passages, %d term occurrences, avg length %.1f, %.1fs\n",
		docs, tokens, avg, elapsed.Seconds())
}
