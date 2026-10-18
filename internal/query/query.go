// Package query evaluates queries using block-based DAAT traversal.
package query

import (
	"fmt"
	"searchindex/internal/finalindex"
	"strings"
)

// Evaluate returns the top ten results for an AND or OR query without printing.
func Evaluate(query, mode string, lexicon map[string]finalindex.Entry, index_reader *finalindex.Reader) ([]Result, error) {
	if mode != "conjunctive" && mode != "disjunctive" {
		return nil, fmt.Errorf("unknown query mode %q", mode)
	}
	query_terms, missing_term, err := loadQueryTerms(query, lexicon, index_reader)
	if err != nil {
		return nil, err
	}

	var results []Result
	if mode == "conjunctive" {
		if missing_term || len(query_terms) == 0 {
			return nil, nil
		}
		results, err = conjunctiveResults(query_terms, index_reader, bm5_rank)
	} else if mode == "disjunctive" {
		// Missing terms contribute zero; process every term that exists.
		results, err = disjunctiveResults(query_terms, index_reader, bm5_rank)
	}
	if err != nil {
		return nil, err
	}

	// Resolve PIDs only for the retained results, not for every candidate.
	for index := range results {
		document, err := index_reader.Document(results[index].DocID)
		if err != nil {
			return nil, err
		}
		results[index].PID = document.PID
	}
	return results, nil
}

// loadQueryTerms loads descriptors and one posting block per distinct query term.
func loadQueryTerms(query string, lexicon map[string]finalindex.Entry, index_reader *finalindex.Reader) ([]queryTerm, bool, error) {
	terms := strings.Fields(strings.ToLower(query))
	query_terms := make([]queryTerm, 0, len(terms))
	seen_terms := make(map[string]bool)
	missing_term := false
	for _, term := range terms {
		if seen_terms[term] {
			continue
		}
		seen_terms[term] = true
		entry, found := lexicon[term]
		if !found {
			missing_term = true
			continue
		}
		descriptors, err := index_reader.BlockDescriptors(entry)
		if err != nil {
			return nil, false, fmt.Errorf("%s descriptors: %w", term, err)
		}
		postings, err := index_reader.ReadPostingBlock(descriptors[0])
		if err != nil {
			return nil, false, fmt.Errorf("%s postings: %w", term, err)
		}
		query_terms = append(query_terms, queryTerm{
			term: term, entry: entry, block_descriptors: descriptors, postings: postings,
		})
	}
	return query_terms, missing_term, nil
}
