## Build:

```sh
cargo build --release
cargo test --release
```

## Running the parser

```sh
# MS MARCO: collection.tsv from collection.tar.gz, unpacked into data/
./target/release/parse --input data/collection.tsv --dir data/rust-full-run

# quick look at what it does
./target/release/parse --input data/collection.tsv --dir /tmp/t --limit 3 --show --ascii
```

| flag | default | meaning |
|---|---|---|
| `--input PATH` | `data/collection.tsv` | `pid<TAB>passage` per line |
| `--dir PATH` | `data` | output directory |
| `--mem-mb N` | `1024` | initial posting-buffer budget in MiB, split equally; minimum 4 |
| `--limit N` | `0` (all) | stop after N passages |
| `--show` | off | print every posting (term, docID, freq) |
| `--ascii` | off | write runs as `.tsv` text instead of binary |

Flags also work with a single dash (`-input`), like the Go version.
Rerunning the parser deletes matching old run files and overwrites the document
table and metadata in the selected output directory.

Output:

```
data/rust-full-run/runs/postings-000000.bin ...   sorted runs (input to merge)
data/rust-full-run/pagetable.bin    docID -> pid, length, file offset
data/rust-full-run/meta.txt         num_docs, avg_doc_len, num_runs, ...
```

Inspect or validate runs:

```sh
./target/release/dumprun --limit 20 data/rust-full-run/runs/postings-000000.bin
./target/release/dumprun --term zebra data/rust-full-run/runs/*.bin
./target/release/dumprun --check data/rust-full-run/runs/*.bin
```

## How it works

This is the lecture's **Merge-Sort Improved** algorithm:

1. Read the collection line by line; assign docIDs 0, 1, 2, ... to valid passage
   lines in file order. Lines without a tab are skipped and receive no docID.
2. Tokenize the passage, then sort its terms locally so repeated terms are
   adjacent. This turns repeated occurrences into a single
   `(term, docID, freq)` posting per distinct term.
3. Append the postings to a large preallocated buffer: an array of fixed
   24-byte records `(8-byte term prefix, docID, freq, offset, length)` plus a
   byte arena holding the term strings. `--mem-mb` budgets the initial capacity
   of both posting buffers, not total process memory. A passage too large for
   an empty buffer can grow it. Input/output buffers and per-document scratch
   space consume additional memory.
4. When the buffer is full, hand it to a background thread that sorts it by
   `(term, docID)` and writes it as one run. The main thread meanwhile keeps
   parsing into a second buffer, which overlaps sorting with parsing.

The sort compares the 8-byte big-endian term prefixes first. Unequal prefixes
avoid comparing the term bytes.

Because docIDs only increase, every docID in run *k+1* is larger than every
docID in run *k*. The merger can build each final inverted list by
concatenating that term's records from run 0, 1, 2, ... It only needs a k-way
merge on terms and never has to re-sort docIDs.

### Tokenization (`src/tokenizer.rs`)

The Rust parser uses these rules:

- ASCII letters and digits form terms, and letters are lowercased.
- Every other ASCII byte is a separator, so `don't` becomes `don`, `t`.
- A token containing any non-ASCII byte is dropped whole. This ignores
  foreign-language text and never fails on malformed UTF-8.
- Tokens longer than 64 bytes are dropped.
- There is no stemming and no stopword removal.

The Go query processor currently lowercases and splits on whitespace instead.
For example, indexing `cat,dog` produces `cat` and `dog`.

### Run file format (`src/runfile.rs`)

```
header:  "IRN1" | num_terms (u64 LE)
record:  term_len | term | count | last_doc | docs_len | freqs_len
         | docID gaps (varbyte, first relative to 0) | freqs (varbyte)
```

All integers in a record are varbyte. Records are in byte order of the term.
DocIDs and frequencies are stored in separate blocks, not interleaved.
The Go merger decodes source gaps and re-encodes output blocks; it uses
`last_doc` to validate each decoded source record.

### Page table (`src/doctable.rs`)

The page table uses fixed 16-byte records, so record *d* sits at byte
`16*d`. Each record holds:

- `pid` (u32)
- `len` (u32), accepted token occurrences including repetitions, used by BM25
- `offset` (u64), the byte position of the passage in the collection file,
  so snippets can be fetched later

## Merge the Rust runs with Go

```sh
go run ./cmd/merge -input data/rust-full-run -dir data/final-index
# Build a standalone executable:
go build -o /tmp/merge-index ./cmd/merge
# Tests for the merger and query primitives:
go test ./cmd/... ./internal/...
```

The output directory must be new. The default block size is 128 postings;
change it with `-block-size N`. The merger writes varbyte-compressed posting
blocks, a binary block directory for docID skipping, a TSV lexicon, and a copy
of the Rust document table for docID-to-PID lookup. The on-disk format is
unchanged by the list API: each 24-byte block descriptor stores the last docID,
posting count, payload offset, document-byte length, and frequency-byte length.
Each block contains varbyte docID gaps followed by varbyte frequencies; its
first docID is absolute.

## Query CLI and package layout

```sh
go run ./cmd/query -dir data/final-index
```

- `cmd/query/main.go`: CLI input, startup wiring, and result printing.
- `internal/query/query.go`: `Evaluate` entry point, term preparation, and PID resolution.
- `internal/query/evaluation.go`: AND/OR DAAT evaluation, BM25, and the top-ten heap.
- `internal/finalindex/list.go`: stateful list cursors, block skipping, and lazy chunk loading.
- `internal/finalindex/lexicon.go`: lexicon loading, called once by `Open`.
- `internal/finalindex/reader.go`: binary index reads and decompression.

BM25 scoring is implemented in `internal/query/evaluation.go` using k1=1.2,
b=0.75 and positive IDF. Collection statistics load once at CLI startup;
candidate document lengths are read on demand. Query tokenization currently
lowercases and splits on whitespace. The existing compressed index format is unchanged.

## Inverted-list API

`finalindex.Open(dir)` opens the binary files and loads the lexicon once.
Query algorithms use `reader.OpenList(term)` to create a cursor, then:

- `list.NextGEQ(target)` returns the first remaining docID at or above the
  target, or `finalindex.EndDocID` (`1 << 32`) when exhausted. Targets and return
  values are `uint64`, so advancing beyond a `uint32` docID cannot overflow.
- `list.GetFreq()` returns the current posting's frequency.
- `list.DocFrequency()` provides the term's document frequency for BM25.
- `list.Close()` releases cursor buffers; `reader.Close()` closes shared files.

Opening a list reads its descriptors but no posting payload. `NextGEQ` skips
blocks using descriptor maxima and reads/decodes only the needed docID chunk.
`GetFreq` reads/decodes that block's frequency chunk on first demand. Same-block
seeks reuse decoded docIDs and frequencies. Each cursor retains at most one
decoded block's docIDs and frequencies, plus all descriptors for its term.
No application-wide posting cache is used.

AND evaluation orders lists shortest first and scores only intersection
members. OR evaluation merges current docIDs and scores each union member
once. Missing terms yield no AND matches and contribute nothing to OR.
Queries return up to ten matches, ranked by descending BM25 score; lower docID
wins equal-score ties. The CLI prints PID, docID, and score without fetching snippets.
All opened cursors close on success or error. No stored impact-score getter is
needed because scoring uses frequencies.

## Build and run all three phases

From the repository root:

```sh
cargo build --release
go build -o /tmp/search-merge ./cmd/merge
go build -o /tmp/search-query ./cmd/query
cargo test --release
go test ./...

# Phase 1: parse into a separate directory to preserve previous runs.
./target/release/parse --input data/collection.tsv --dir data/rust-api-check

# Phase 2: choose a new output directory; the merger refuses an existing one.
/tmp/search-merge -input data/rust-api-check -dir data/final-index-api

# Phase 3: choose a mode separately for each query.
/tmp/search-query -dir data/final-index-api
```

For a quick rebuild, append `--limit 20000 --mem-mb 4` to phase 1 and use
separate directories such as `data/rust-sample` and `data/final-sample`.
The parser must use binary output (omit `--ascii`). Existing final indexes
remain compatible: run `/tmp/search-query -dir data/final-index` to query one
without rebuilding.

Example interactive input:

```text
Query> cat dog
Mode [conjunctive/disjunctive]> conjunctive
Query> cat dog
Mode [conjunctive/disjunctive]> disjunctive
Query> :quit
```
