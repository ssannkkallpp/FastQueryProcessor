package query

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"searchindex/internal/finalindex"
	"sort"
	"testing"
)

type fakeBlockReader struct {
	blocks map[uint64][]finalindex.Posting
	reads  []uint64
	fail   bool
}

func (reader *fakeBlockReader) ReadPostingBlock(descriptor finalindex.BlockDescriptor) ([]finalindex.Posting, error) {
	if reader.fail {
		return nil, errors.New("read failed")
	}
	reader.reads = append(reader.reads, descriptor.PayloadOffset)
	return reader.blocks[descriptor.PayloadOffset], nil
}
func fakeTerm(reader *fakeBlockReader, postings []finalindex.Posting, blockSize int) queryTerm {
	var descriptors []finalindex.BlockDescriptor
	for start := 0; start < len(postings); start += blockSize {
		end := start + blockSize
		if end > len(postings) {
			end = len(postings)
		}
		offset := uint64(len(reader.blocks) + 4)
		reader.blocks[offset] = append([]finalindex.Posting(nil), postings[start:end]...)
		descriptors = append(descriptors, finalindex.BlockDescriptor{LastDocID: postings[end-1].DocID, PostingCount: uint32(end - start), PayloadOffset: offset})
	}
	return queryTerm{block_descriptors: descriptors, postings: reader.blocks[descriptors[0].PayloadOffset]}
}
func TestAdvanceSkipsBlocksAndReusesCurrentBlock(t *testing.T) {
	reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
	term := fakeTerm(reader, []finalindex.Posting{{DocID: 2, Frequency: 1}, {DocID: 10, Frequency: 2}, {DocID: 20, Frequency: 1}, {DocID: 30, Frequency: 1}, {DocID: 100, Frequency: 3}, {DocID: 120, Frequency: 1}}, 2)
	if err := term.Advance(reader, 10); err != nil {
		t.Fatal(err)
	}
	if term.current().DocID != 10 || len(reader.reads) != 0 {
		t.Fatal("should reuse initial block")
	}
	if err := term.Advance(reader, 99); err != nil {
		t.Fatal(err)
	}
	if term.current().DocID != 100 || term.current_block != 2 || len(reader.reads) != 1 {
		t.Fatalf("failed to skip block: %+v", reader.reads)
	}
	if err := term.Advance(reader, 50); err != nil {
		t.Fatal(err)
	}
	if term.current().DocID != 100 || len(reader.reads) != 1 {
		t.Fatal("must not move backward or reread")
	}
	if err := term.Next(reader); err != nil {
		t.Fatal(err)
	}
	if term.current().DocID != 120 {
		t.Fatal("next within block")
	}
	if err := term.Next(reader); err != nil || !term.exhausted {
		t.Fatal("expected exhaustion", err)
	}
}
func TestConjunctiveTopTenAgainstReference(t *testing.T) {
	random := rand.New(rand.NewSource(42))
	for trial := 0; trial < 80; trial++ {
		reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
		var terms []queryTerm
		occurrence_count := map[uint32]int{}
		reference_scores := map[uint32]float64{}
		term_count := 1 + random.Intn(5)
		for term_index := 0; term_index < term_count; term_index++ {
			var postings []finalindex.Posting
			for doc := uint32(0); doc < 200; doc++ {
				if random.Intn(3) != 0 {
					frequency := uint32(1 + random.Intn(5))
					postings = append(postings, finalindex.Posting{DocID: doc, Frequency: frequency})
					occurrence_count[doc]++
					reference_scores[doc] += float64(frequency)
				}
			}
			terms = append(terms, fakeTerm(reader, postings, 1+random.Intn(9)))
		}
		score_calls := map[uint32]int{}
		rank := func(doc uint32, terms []queryTerm) float64 {
			score_calls[doc]++
			var score float64
			for _, term := range terms {
				if term.current().DocID != doc {
					t.Fatal("scored unaligned lists")
				}
				score += float64(term.current().Frequency)
			}
			return score
		}
		got, err := conjunctiveResults(terms, reader, rank)
		if err != nil {
			t.Fatal(err)
		}
		var want []Result
		matches := 0
		for doc, count := range occurrence_count {
			if count == term_count {
				matches++
				if score_calls[doc] != 1 {
					t.Fatalf("doc %d scored %d times", doc, score_calls[doc])
				}
				want = append(want, Result{DocID: doc, Score: reference_scores[doc]})
			}
		}
		if len(score_calls) != matches {
			t.Fatal("scored documents outside intersection")
		}
		sort.Slice(want, func(i, j int) bool {
			if want[i].Score == want[j].Score {
				return want[i].DocID < want[j].DocID
			}
			return want[i].Score > want[j].Score
		})
		if len(want) > 10 {
			want = want[:10]
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: got %+v want %+v", trial, got, want)
		}
	}
}
func TestConjunctiveNoMatchesAndReadError(t *testing.T) {
	reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
	first := fakeTerm(reader, []finalindex.Posting{{DocID: 0, Frequency: 1}, {DocID: 2, Frequency: 1}}, 1)
	second := fakeTerm(reader, []finalindex.Posting{{DocID: 1, Frequency: 1}, {DocID: 3, Frequency: 1}}, 1)
	rank := func(uint32, []queryTerm) float64 { t.Fatal("must not score nonmatches"); return 0 }
	results, err := conjunctiveResults([]queryTerm{first, second}, reader, rank)
	if err != nil || len(results) != 0 {
		t.Fatalf("nonmatching lists: %+v %v", results, err)
	}
	reader.fail = true
	if err := first.Next(reader); err == nil {
		t.Fatal("expected block read error")
	}
}
func TestPlaceholderRanking(t *testing.T) {
	reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
	var postings []finalindex.Posting
	for doc := uint32(0); doc < 25; doc++ {
		postings = append(postings, finalindex.Posting{DocID: doc, Frequency: 1})
	}
	term := fakeTerm(reader, postings, 2)
	results, err := conjunctiveResults([]queryTerm{term}, reader, bm5_rank)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 10 {
		t.Fatal("expected ten results")
	}
	for i, result := range results {
		if result.DocID != uint32(i) || result.Score != 0 {
			t.Fatal("unexpected placeholder tie order")
		}
	}
}

func TestDisjunctiveTopTenAgainstReference(t *testing.T) {
	random := rand.New(rand.NewSource(84))
	for trial := 0; trial < 80; trial++ {
		reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
		var query_terms []queryTerm
		reference_scores := map[uint32]float64{}
		reference_terms := map[uint32]int{}
		term_count := 1 + random.Intn(5)
		for term_index := 0; term_index < term_count; term_index++ {
			var postings []finalindex.Posting
			for doc_id := uint32(0); doc_id < 200; doc_id++ {
				if random.Intn(3) != 0 {
					frequency := uint32(1 + random.Intn(5))
					postings = append(postings, finalindex.Posting{DocID: doc_id, Frequency: frequency})
					reference_scores[doc_id] += float64(frequency)
					reference_terms[doc_id]++
				}
			}
			query_terms = append(query_terms, fakeTerm(reader, postings, 1+random.Intn(9)))
		}
		score_calls := map[uint32]int{}
		var previous_doc_id uint32
		rank := func(doc_id uint32, matching_terms []queryTerm) float64 {
			if len(score_calls) > 0 && doc_id <= previous_doc_id {
				t.Fatal("candidates must be strictly increasing")
			}
			previous_doc_id = doc_id
			score_calls[doc_id]++
			if len(matching_terms) != reference_terms[doc_id] {
				t.Fatalf("doc %d: missing matching terms", doc_id)
			}
			var score float64
			for _, term := range matching_terms {
				if term.exhausted || term.current().DocID != doc_id {
					t.Fatal("scored a nonmatching term")
				}
				score += float64(term.current().Frequency)
			}
			return score
		}
		results, err := disjunctiveResults(query_terms, reader, rank)
		if err != nil {
			t.Fatal(err)
		}
		var expected []Result
		for doc_id, score := range reference_scores {
			if score_calls[doc_id] != 1 {
				t.Fatalf("doc %d scored %d times", doc_id, score_calls[doc_id])
			}
			expected = append(expected, Result{DocID: doc_id, Score: score})
		}
		if len(score_calls) != len(reference_scores) {
			t.Fatal("candidate union mismatch")
		}
		sort.Slice(expected, func(i, j int) bool {
			if expected[i].Score == expected[j].Score {
				return expected[i].DocID < expected[j].DocID
			}
			return expected[i].Score > expected[j].Score
		})
		if len(expected) > 10 {
			expected = expected[:10]
		}
		if !reflect.DeepEqual(results, expected) {
			t.Fatalf("trial %d: got %+v, want %+v", trial, results, expected)
		}
	}
}

func TestDisjunctiveContinuesAfterExhaustion(t *testing.T) {
	reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
	first := fakeTerm(reader, []finalindex.Posting{{DocID: 0, Frequency: 2}}, 1)
	second := fakeTerm(reader, []finalindex.Posting{{DocID: 0, Frequency: 3}, {DocID: 5, Frequency: 4}, {DocID: math.MaxUint32, Frequency: 1}}, 1)
	var candidates []uint32
	rank := func(doc_id uint32, matching_terms []queryTerm) float64 {
		candidates = append(candidates, doc_id)
		var score float64
		for _, term := range matching_terms {
			score += float64(term.current().Frequency)
		}
		return score
	}
	results, err := disjunctiveResults([]queryTerm{first, second}, reader, rank)
	if err != nil {
		t.Fatal(err)
	}
	expected := []Result{{DocID: 0, Score: 5}, {DocID: 5, Score: 4}, {DocID: math.MaxUint32, Score: 1}}
	if !reflect.DeepEqual(results, expected) || !reflect.DeepEqual(candidates, []uint32{0, 5, math.MaxUint32}) {
		t.Fatalf("results %+v, candidates %v", results, candidates)
	}
	// Only the second list's later blocks should have been read, once each.
	if len(reader.reads) != 2 {
		t.Fatalf("unexpected payload reads: %v", reader.reads)
	}
}

func TestDisjunctiveEmptyAndReadError(t *testing.T) {
	reader := &fakeBlockReader{blocks: map[uint64][]finalindex.Posting{}}
	rank := func(uint32, []queryTerm) float64 { return 0 }
	results, err := disjunctiveResults(nil, reader, rank)
	if err != nil || len(results) != 0 {
		t.Fatalf("empty query: %+v %v", results, err)
	}
	term := fakeTerm(reader, []finalindex.Posting{{DocID: 1, Frequency: 1}, {DocID: 2, Frequency: 1}}, 1)
	reader.fail = true
	if _, err := disjunctiveResults([]queryTerm{term}, reader, rank); err == nil {
		t.Fatal("expected block read error")
	}
}

func TestLoadQueryTermsAllMissing(t *testing.T) {
	terms, missing_term, err := loadQueryTerms("unknown unknown", map[string]finalindex.Entry{}, nil)
	if err != nil || !missing_term || len(terms) != 0 {
		t.Fatalf("missing terms: %+v %v %v", terms, missing_term, err)
	}
}
