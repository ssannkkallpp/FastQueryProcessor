package main

import (
	"flag"
	"fmt"
	"log"
	"searchindex/internal/finalindex"
	"time"
)

func main() {
	input_path := flag.String("input", "data/rust-full-run", "Rust parser output directory (containing runs/, pagetable.bin, meta.txt)")
	output_path := flag.String("dir", "data/final-index", "new final index directory; must not exist")
	blockSize := flag.Int("block-size", 128, "postings per independently decodable block")
	flag.Parse()
	start := time.Now()
	stats, err := finalindex.Merge(*input_path, *output_path, *blockSize)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("merged %d terms, %d postings, %d blocks into %s in %s\n", stats.Terms, stats.Postings, stats.Blocks, *output_path, time.Since(start).Round(time.Millisecond))
}
