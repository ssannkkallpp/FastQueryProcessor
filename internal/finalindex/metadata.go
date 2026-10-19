package finalindex

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// CollectionStats supplies the collection-wide inputs needed by BM25.
type CollectionStats struct {
	NumDocs      uint64
	AvgDocLength float64
}

// LoadCollectionStats reads the Rust collection metadata once at startup.
func LoadCollectionStats(path string) (CollectionStats, error) {
	metadata, err := os.ReadFile(path)
	if err != nil {
		return CollectionStats{}, err
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(metadata), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			fields[key] = value
		}
	}
	document_count, err := strconv.ParseUint(fields["num_docs"], 10, 32)
	if err != nil {
		return CollectionStats{}, fmt.Errorf("invalid num_docs: %w", err)
	}
	average_length, err := strconv.ParseFloat(fields["avg_doc_len"], 64)
	if err != nil {
		return CollectionStats{}, fmt.Errorf("invalid avg_doc_len: %w", err)
	}
	if math.IsNaN(average_length) || math.IsInf(average_length, 0) || average_length < 0 || document_count == 0 && average_length != 0 {
		return CollectionStats{}, fmt.Errorf("invalid collection document count or average length")
	}
	return CollectionStats{NumDocs: document_count, AvgDocLength: average_length}, nil
}
