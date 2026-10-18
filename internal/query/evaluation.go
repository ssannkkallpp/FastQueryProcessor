package query

import (
	"container/heap"
	"fmt"
	"searchindex/internal/finalindex"
	"sort"
)

// postingBlockReader allows iterators to load just one block at a time.
type postingBlockReader interface {
	ReadPostingBlock(finalindex.BlockDescriptor) ([]finalindex.Posting, error)
}

// queryTerm retains one decoded block and a forward-only cursor for a term.
type queryTerm struct {
	term              string
	entry             finalindex.Entry
	block_descriptors []finalindex.BlockDescriptor
	current_block     int
	postings          []finalindex.Posting
	current_posting   int
	exhausted         bool
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

func (term *queryTerm) current() finalindex.Posting { return term.postings[term.current_posting] }

// loadBlock replaces the previous block; it does not accumulate decoded lists.
func (term *queryTerm) loadBlock(reader postingBlockReader, block_index int) error {
	if block_index >= len(term.block_descriptors) {
		term.exhausted = true
		term.postings = nil
		return nil
	}
	postings, err := reader.ReadPostingBlock(term.block_descriptors[block_index])
	if err != nil {
		return fmt.Errorf("%s block %d: %w", term.term, block_index, err)
	}
	term.current_block = block_index
	term.current_posting = 0
	term.postings = postings
	return nil
}

// Next advances once, loading the next block only when the current one ends.
func (term *queryTerm) Next(reader postingBlockReader) error {
	if term.exhausted {
		return nil
	}
	term.current_posting++
	if term.current_posting < len(term.postings) {
		return nil
	}
	return term.loadBlock(reader, term.current_block+1)
}

// Advance moves forward to docID >= target, skipping blocks by their maxima.
func (term *queryTerm) Advance(reader postingBlockReader, target uint32) error {
	if term.exhausted || term.current().DocID >= target {
		return nil
	}
	if term.block_descriptors[term.current_block].LastDocID < target {
		remaining_start := term.current_block + 1
		skipped := sort.Search(len(term.block_descriptors)-remaining_start, func(index int) bool {
			return term.block_descriptors[remaining_start+index].LastDocID >= target
		})
		if err := term.loadBlock(reader, remaining_start+skipped); err != nil {
			return err
		}
		if term.exhausted {
			return nil
		}
	}
	// Search only the remaining postings in the current decoded block.
	remaining := term.postings[term.current_posting:]
	term.current_posting += sort.Search(len(remaining), func(index int) bool { return remaining[index].DocID >= target })
	return nil
}

// conjunctiveResults aligns all lists, scores each intersection once, and keeps ten.
func conjunctiveResults(query_terms []queryTerm, reader postingBlockReader, rank func(uint32, []queryTerm) float64) ([]Result, error) {
	var query_results resultHeap
	if len(query_terms) == 0 {
		return nil, nil
	}
	candidate_doc_id := query_terms[0].current().DocID
	for {
		documents_aligned := true
		for index := range query_terms {
			term := &query_terms[index]
			if err := term.Advance(reader, candidate_doc_id); err != nil {
				return nil, err
			}
			if term.exhausted {
				return sortedResults(query_results), nil
			}

			current_doc_id := term.current().DocID
			if current_doc_id > candidate_doc_id {
				candidate_doc_id = current_doc_id
				documents_aligned = false
			}
		}
		// A later list raised the target; revisit earlier lists before scoring.
		if !documents_aligned {
			continue
		}
		result := Result{DocID: candidate_doc_id, Score: rank(candidate_doc_id, query_terms)}
		retainResult(&query_results, result)
		// Move every list beyond this match, stopping as soon as one is exhausted.
		for index := range query_terms {
			if err := query_terms[index].Next(reader); err != nil {
				return nil, err
			}
			if query_terms[index].exhausted {
				return sortedResults(query_results), nil
			}
		}
		candidate_doc_id = query_terms[0].current().DocID
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

// bm5_rank is a scoring placeholder; query_terms contains only terms matching doc_id.
// TODO: Implement BM25 using frequencies, document lengths and collection statistics.
func bm5_rank(doc_id uint32, query_terms []queryTerm) float64 {
	return float64(0.0) // Return dummy for now
}

// Process the smallest current docID, scoring it once using only matching terms.
func disjunctiveResults(query_terms []queryTerm, reader postingBlockReader, rank func(uint32, []queryTerm) float64) ([]Result, error) {
	var query_results resultHeap
	matching_terms := make([]queryTerm, 0, len(query_terms))

	for {
		var candidate_doc_id uint32
		candidate_found := false
		for index := range query_terms {
			term := &query_terms[index]
			if term.exhausted {
				continue
			}
			current_doc_id := term.current().DocID
			if !candidate_found || current_doc_id < candidate_doc_id {
				candidate_doc_id = current_doc_id
				candidate_found = true
			}
		}
		if !candidate_found {
			return sortedResults(query_results), nil
		}

		// Other terms are ahead of this candidate or exhausted, so contribute zero.
		matching_terms = matching_terms[:0]
		for index := range query_terms {
			term := &query_terms[index]
			if !term.exhausted && term.current().DocID == candidate_doc_id {
				matching_terms = append(matching_terms, *term)
			}
		}
		result := Result{DocID: candidate_doc_id, Score: rank(candidate_doc_id, matching_terms)}
		retainResult(&query_results, result)

		// Advance only the lists that contributed. Exhausted lists do not end OR.
		for index := range query_terms {
			term := &query_terms[index]
			if !term.exhausted && term.current().DocID == candidate_doc_id {
				if err := term.Next(reader); err != nil {
					return nil, err
				}
			}
		}
	}
}
