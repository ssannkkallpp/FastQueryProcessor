// Package query evaluates queries through the inverted-list cursor API.
package query

import (
	"errors"
	"fmt"
	"searchindex/internal/finalindex"
	"sort"
	"strings"
)

// Evaluate returns up to ten BM25-ranked results for an AND or OR query.
func Evaluate(text, mode string, reader *finalindex.Reader, statistics finalindex.CollectionStats) ([]Result, error) {
	if mode != "conjunctive" && mode != "disjunctive" {
		return nil, fmt.Errorf("unknown query mode %q", mode)
	}
	lists, missing, err := openQueryLists(text, reader)
	if err != nil {
		return nil, err
	}
	defer closeLists(lists)
	if mode == "conjunctive" && missing {
		return nil, nil
	}
	rank := func(doc uint32, matching []listCursor) (float64, error) {
		return bm25Score(doc, matching, reader, statistics)
	}
	var results []Result
	if mode == "conjunctive" {
		sort.SliceStable(lists, func(i, j int) bool { return lists[i].DocFrequency() < lists[j].DocFrequency() })
		results, err = conjunctiveResults(lists, rank)
	} else {
		results, err = disjunctiveResults(lists, rank)
	}
	if err != nil {
		return nil, err
	}
	for i := range results {
		document, err := reader.Document(results[i].DocID)
		if err != nil {
			return nil, err
		}
		results[i].PID = document.PID
	}
	return results, nil
}

// Opening a query establishes list cursors without decompressing any postings.
// Missing terms end AND queries and contribute nothing to OR queries.
func openQueryLists(text string, reader *finalindex.Reader) ([]listCursor, bool, error) {
	var lists []listCursor
	seen := make(map[string]bool)
	missing := false
	for _, term := range strings.Fields(strings.ToLower(text)) {
		if seen[term] {
			continue
		}
		seen[term] = true
		list, err := reader.OpenList(term)
		if errors.Is(err, finalindex.ErrTermNotFound) {
			missing = true
			continue
		}
		if err != nil {
			closeLists(lists)
			return nil, false, err
		}
		lists = append(lists, list)
	}
	return lists, missing, nil
}

func closeLists(lists []listCursor) {
	for _, list := range lists {
		list.Close()
	}
}
