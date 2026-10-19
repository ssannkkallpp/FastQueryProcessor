package query

import (
	"container/heap"
	"fmt"
	"math"
	"searchindex/internal/finalindex"
	"sort"
)

const K1 = 1.2
const B = 0.75

// documentReader retrieves one candidate's length without loading the page table.
type documentReader interface {
	Document(uint32) (finalindex.Document, error)
}

// listCursor is the complete posting-list boundary used by query algorithms.
// Storage, descriptors, decompression, and buffer state stay inside finalindex.
type listCursor interface {
	NextGEQ(uint64) (uint64, error)
	GetFreq() (uint32, error)
	DocFrequency() uint64
	Close()
}

// Result contains a retained document, its original PID, and its score.
type Result struct {
	DocID, PID uint32
	Score      float64
}

// resultHeap keeps the worst retained result at its root; lower docID wins ties.
type resultHeap []Result

func (results resultHeap) Len() int { return len(results) }
func (results resultHeap) Less(i, j int) bool {
	if results[i].Score == results[j].Score {
		return results[i].DocID > results[j].DocID
	}
	return results[i].Score < results[j].Score
}
func (results resultHeap) Swap(i, j int)   { results[i], results[j] = results[j], results[i] }
func (results *resultHeap) Push(value any) { *results = append(*results, value.(Result)) }
func (results *resultHeap) Pop() any {
	last := len(*results) - 1
	value := (*results)[last]
	*results = (*results)[:last]
	return value
}

// conjunctiveResults aligns lists in shortest-first order, scoring only matches.
func conjunctiveResults(lists []listCursor, rank func(uint32, []listCursor) (float64, error)) ([]Result, error) {
	var results resultHeap
	if len(lists) == 0 {
		return nil, nil
	}
	var target uint64
	for {
		candidate, err := lists[0].NextGEQ(target)
		if err != nil {
			return nil, err
		}
		if candidate == finalindex.EndDocID {
			return sortedResults(results), nil
		}
		aligned := true
		for _, list := range lists[1:] {
			doc, err := list.NextGEQ(candidate)
			if err != nil {
				return nil, err
			}
			if doc == finalindex.EndDocID {
				return sortedResults(results), nil
			}
			if doc > candidate {
				target = doc
				aligned = false
				break
			}
		}
		if !aligned {
			continue
		}
		score, err := rank(uint32(candidate), lists)
		if err != nil {
			return nil, err
		}
		retainResult(&results, Result{DocID: uint32(candidate), Score: score})
		target = candidate + 1
	}
}

// Keep at most ten results, with the worst retained result at the heap root.
func retainResult(query_results *resultHeap, result Result) {
	if query_results.Len() < 10 {
		heap.Push(query_results, result)
	} else if result.Score > (*query_results)[0].Score || result.Score == (*query_results)[0].Score && result.DocID < (*query_results)[0].DocID {
		(*query_results)[0] = result
		heap.Fix(query_results, 0)
	}
}

// Comparison function for sorting the results
func sortedResults(results resultHeap) []Result {
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].DocID < results[j].DocID
		}
		return results[i].Score > results[j].Score
	})
	return []Result(results)
}

// disjunctiveResults merges current docIDs and scores every union member once.
func disjunctiveResults(lists []listCursor, rank func(uint32, []listCursor) (float64, error)) ([]Result, error) {
	var results resultHeap
	current := make([]uint64, len(lists))
	for i, list := range lists {
		doc, err := list.NextGEQ(0)
		if err != nil {
			return nil, err
		}
		current[i] = doc
	}
	matching := make([]listCursor, 0, len(lists))
	for {
		candidate := finalindex.EndDocID
		for _, doc := range current {
			if doc < candidate {
				candidate = doc
			}
		}
		if candidate == finalindex.EndDocID {
			return sortedResults(results), nil
		}
		matching = matching[:0]
		for i, list := range lists {
			if current[i] == candidate {
				matching = append(matching, list)
			}
		}
		score, err := rank(uint32(candidate), matching)
		if err != nil {
			return nil, err
		}
		retainResult(&results, Result{DocID: uint32(candidate), Score: score})
		for i, list := range lists {
			if current[i] == candidate {
				doc, err := list.NextGEQ(candidate + 1)
				if err != nil {
					return nil, err
				}
				current[i] = doc
			}
		}
	}
}

func bm25Score(doc_id uint32, matching_terms []listCursor, reader documentReader, statistics finalindex.CollectionStats) (float64, error) {
	if len(matching_terms) == 0 {
		return 0, nil
	}

	if statistics.NumDocs == 0 || statistics.AvgDocLength <= 0 || math.IsNaN(statistics.AvgDocLength) || math.IsInf(statistics.AvgDocLength, 0) {
		return 0, fmt.Errorf("BM25 requires a positive document count and average document length")
	}

	document, err := reader.Document(doc_id)
	if err != nil {
		return 0, fmt.Errorf("document %d: %w", doc_id, err)
	}
	if uint64(doc_id) >= statistics.NumDocs || document.Length == 0 {
		return 0, fmt.Errorf("invalid scoring document %d or document length", doc_id)
	}

	length_normalization := K1 * (1 - B + B*float64(document.Length)/statistics.AvgDocLength)
	var score float64
	for _, term := range matching_terms {
		frequency, err := term.GetFreq()
		if err != nil {
			return 0, err
		}
		document_frequency := term.DocFrequency()
		if frequency == 0 || document_frequency == 0 || document_frequency > statistics.NumDocs {
			return 0, fmt.Errorf("invalid BM25 frequency or document frequency")
		}
		// Positive IDF variant: ln(1 + (N - df + 0.5)/(df + 0.5)).
		inverse_document_frequency := math.Log1p((float64(statistics.NumDocs-document_frequency) + 0.5) / (float64(document_frequency) + 0.5))
		term_frequency := float64(frequency)
		score += inverse_document_frequency * term_frequency * (K1 + 1) / (term_frequency + length_normalization)
	}
	return score, nil
}
