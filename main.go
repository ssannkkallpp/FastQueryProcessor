// main.go: parse unpacked MS MARCO passage collection.
//
// Usage:
//
//	go run main.go -limit 1000 -show
//	go run main.go -input data/collection.tsv
//
// Output: pagetable.tsv in -dir (default ./data), containing
// docID <TAB> pid <TAB> length_in_terms.
// Terms are passed to the Emit callback for a future index builder.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Terms longer than this are dropped - Need to edit
const maxTermLen = 64

// Emit receives one posting per term occurrence. The term slice is reused
// between calls: copy it if you need to keep it.
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
	dir := flag.String("dir", "data", "output directory for pagetable.tsv")
	limit := flag.Int("limit", 0, "parse only the first N passages (0 = all)")
	show := flag.Bool("show", false, "print every posting (use with a small -limit)")
	flag.Parse()

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	fmt.Println("parsing", *input)
	start := time.Now()

	emit := func(term []byte, docID uint32) {
		if *show {
			fmt.Printf("%-64s %d\n", term, docID)
		}
	}

	docs, tokens, err := parse(*input, filepath.Join(*dir, "pagetable.tsv"), *limit, emit)
	if err != nil {
		log.Fatal("parse: ", err)
	}
	avg := 0.0
	if docs > 0 {
		avg = float64(tokens) / float64(docs)
	}
	fmt.Printf("done: %d passages, %d term occurrences, avg length %.1f, %.1fs\n",
		docs, tokens, avg, time.Since(start).Seconds())
}
