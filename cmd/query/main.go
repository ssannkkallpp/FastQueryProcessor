package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"searchindex/internal/finalindex"
	queryengine "searchindex/internal/query"
	"strings"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	indexDir := flag.String("dir", "data/final-index", "final index directory")
	flag.Parse()

	// Open the binary files and load the lexicon once for all queries
	index_reader, err := finalindex.Open(*indexDir)
	if err != nil {
		return err
	}
	defer index_reader.Close()

	// Read metadata to store average document length and number of documents
	statistics, err := finalindex.LoadCollectionStats(filepath.Join(*indexDir, "collection-meta.txt"))
	if err != nil {
		return err
	}

	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), 1<<20)
	queryCache := make(map[string][]queryengine.Result)
	fmt.Println("Enter a query, or :quit to exit")

	for {
		fmt.Print("Query> ")
		if !input.Scan() {
			fmt.Println("Error reading the query")
			return input.Err()
		}

		query := strings.TrimSpace(input.Text())
		if query == ":quit" {
			return nil
		}

		if query == "" {
			continue
		}

		var mode string
		for {
			fmt.Print("Mode [conjunctive/disjunctive]> ")
			if !input.Scan() {
				fmt.Println()
				return input.Err()
			}
			mode = strings.ToLower(strings.TrimSpace(input.Text()))
			if mode == ":quit" {
				return nil
			}
			if mode == "conjunctive" || mode == "disjunctive" {
				break
			}
			fmt.Println("Please enter conjunctive or disjunctive.")
		}

		if err := processQuery(query, mode, index_reader, statistics, queryCache); err != nil {
			return err
		}
	}
}

// Process the given query and hand off final results to the print helper
func processQuery(query_text, mode string, index_reader *finalindex.Reader, statistics finalindex.CollectionStats, cache map[string][]queryengine.Result) error {
	// Include the mode so AND and OR results are cached separately
	key := mode + "\x00" + strings.Join(strings.Fields(strings.ToLower(query_text)), " ")
	if results, found := cache[key]; found {
		printQueryOutput(results)
		return nil
	}
	results, err := queryengine.Evaluate(query_text, mode, index_reader, statistics)
	if err != nil {
		return err
	}
	cache[key] = results
	printQueryOutput(results)
	return nil
}

// Format and print final results
func printQueryOutput(results []queryengine.Result) {
	if len(results) == 0 {
		fmt.Println("No results.")
		return
	}
	for index, result := range results {
		fmt.Printf("%d. PID %d (docID %d), score %.4f\n", index+1, result.PID, result.DocID, result.Score)
	}
}
