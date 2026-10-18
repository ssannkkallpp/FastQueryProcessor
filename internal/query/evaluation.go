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
func conjunctiveResults(query_terms []queryTerm, reader postingBlockReader, rank func(uint32, []queryTerm) (float64, error)) ([]Result, error) {
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

		score, err := rank(candidate_doc_id, query_terms)
		if err != nil {
			return nil, err
		}

		result := Result{DocID: candidate_doc_id, Score: score}
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

// Process the smallest current docID, scoring it once using only matching terms.
func disjunctiveResults(query_terms []queryTerm, reader postingBlockReader, rank func(uint32, []queryTerm) (float64, error)) ([]Result, error) {
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

		score, err := rank(candidate_doc_id, matching_terms)
		if err != nil {
			return nil, err
		}

		result := Result{DocID: candidate_doc_id, Score: score}
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

func bm25Score(doc_id uint32, matching_terms []queryTerm, reader documentReader, statistics finalindex.CollectionStats) (float64, error) {
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
		posting := term.current()
		document_frequency := term.entry.DocFrequency
		if posting.DocID != doc_id || posting.Frequency == 0 || document_frequency == 0 || document_frequency > statistics.NumDocs {
			return 0, fmt.Errorf("invalid BM25 posting or document frequency for term %q", term.term)
		}
		// Positive IDF variant: ln(1 + (N - df + 0.5)/(df + 0.5)).
		inverse_document_frequency := math.Log1p((float64(statistics.NumDocs-document_frequency) + 0.5) / (float64(document_frequency) + 0.5))
		term_frequency := float64(posting.Frequency)
		score += inverse_document_frequency * term_frequency * (K1 + 1) / (term_frequency + length_normalization)
	}
	return score, nil
}
