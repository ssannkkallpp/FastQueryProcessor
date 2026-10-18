package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
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
	lexicon, err := finalindex.LoadLexicon("data/final-index/lexicon.tsv")
	if err != nil {
		return err
	}
	index_reader, err := finalindex.Open("data/final-index")
	if err != nil {
		return err
	}
	defer index_reader.Close()

	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), 1<<20)
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

		if err := processQuery(query, mode, lexicon, index_reader); err != nil {
			return err
		}
	}
}

// Process the given query and print only the final results.
func processQuery(query_text, mode string, lexicon map[string]finalindex.Entry, index_reader *finalindex.Reader) error {
	results, err := queryengine.Evaluate(query_text, mode, lexicon, index_reader)
	if err != nil {
		return err
	}
	printQueryOutput(results)
	return nil
}

// printQueryOutput formats only final results; all index reads happen beforehand.
func printQueryOutput(results []queryengine.Result) {
	fmt.Println("Scores are placeholders until bm5_rank is implemented.")
	if len(results) == 0 {
		fmt.Println("No results.")
		return
	}
	for index, result := range results {
		fmt.Printf("%d. PID %d (docID %d), score %.4f\n", index+1, result.PID, result.DocID, result.Score)
	}
}
