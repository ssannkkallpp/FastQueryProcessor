package main

import (
	"bufio"
	"fmt"
	"os"
	"searchindex/internal/finalindex"
)

// Reads lexicon.tsv once at startup into a term lookup map
func loadLexicon(path string) (map[string]finalindex.Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s: missing lexicon header", path)
	}

	if scanner.Text() != "term\tdoc_frequency\tblocks_offset\tblock_count" {
		return nil, fmt.Errorf("%s: invalid lexicon header", path)
	}

	lexicon := make(map[string]finalindex.Entry)
	line_number := 1
	for scanner.Scan() {
		line_number++
		entry, err := finalindex.ParseEntry(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line_number, err)
		}
		if _, exists := lexicon[entry.Term]; exists {
			return nil, fmt.Errorf("%s line %d: duplicate term %q", path, line_number, entry.Term)
		}
		lexicon[entry.Term] = entry
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lexicon, nil
}
