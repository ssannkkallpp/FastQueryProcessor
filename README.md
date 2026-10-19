## Build:

```sh
cargo build --release
cargo test --release
```

## Running the parser

```sh
# MS MARCO: collection.tsv from collection.tar.gz, unpacked into data/
./target/release/parse --input data/collection.tsv --dir data

# quick look at what it does
./target/release/parse --input data/collection.tsv --dir /tmp/t --limit 3 --show --ascii
```

| flag | default | meaning |
|---|---|---|
| `--input PATH` | `data/collection.tsv` | `pid<TAB>passage` per line |
| `--dir PATH` | `data` | output directory |
| `--mem-mb N` | `1024` | RAM for posting buffers (two buffers of N/2) |
| `--limit N` | `0` (all) | stop after N passages |
| `--show` | off | print every posting (term, docID, freq) |
| `--ascii` | off | write runs as `.tsv` text instead of binary |

Flags also work with a single dash (`-input`), like the Go version.

Output:

```
data/runs/postings-000000.bin ...   sorted runs (input to merge)
data/pagetable.bin                  docID -> pid, length, file offset
data/meta.txt                       num_docs, avg_doc_len, num_runs, ...
```

Inspect or validate runs:

```sh
./target/release/dumprun --limit 20 data/runs/postings-000000.bin
./target/release/dumprun --term zebra data/runs/*.bin
./target/release/dumprun --check data/runs/*.bin
```

## How it works

This is the lecture's **Merge-Sort Improved** algorithm, not a dynamic
in-memory index (DIMDS):

1. Read the collection line by line; assign docIDs 0, 1, 2, ... in file order.
2. Tokenize the passage, then sort its terms locally so repeated terms are
   adjacent. This turns repeated occurrences into a single
   `(term, docID, freq)` posting per distinct term.
3. Append the postings to a large preallocated buffer: an array of fixed
   24-byte records `(8-byte term prefix, docID, freq, offset, length)` plus a
   byte arena holding the term strings. The buffer never grows, so memory use
   is fixed by `--mem-mb`.
4. When the buffer is full, hand it to a background thread that sorts it by
   `(term, docID)` and writes it as one run. The main thread meanwhile keeps
   parsing into a second buffer, which overlaps sorting with parsing.

The sort compares the 8-byte big-endian term prefixes first. Most
comparisons therefore never touch the term bytes, which matters because the
buffer is far larger than the CPU caches.

Because docIDs only increase, every docID in run *k+1* is larger than every
docID in run *k*. The merger can build each final inverted list by
concatenating that term's records from run 0, 1, 2, ... It only needs a k-way
merge on terms and never has to re-sort docIDs.

### Tokenization (`src/tokenizer.rs`)

The rules are identical to the Go reference, and the query processor will
reuse the same function:

- ASCII letters and digits form terms, and letters are lowercased.
- Every other ASCII byte is a separator, so `don't` becomes `don`, `t`.
- A token containing any non-ASCII byte is dropped whole. This ignores
  foreign-language text and never fails on malformed UTF-8.
- Tokens longer than 64 bytes are dropped.
- There is no stemming and no stopword removal.

### Run file format (`src/runfile.rs`)

```
header:  "IRN1" | num_terms (u64 LE)
record:  term_len | term | count | last_doc | docs_len | freqs_len
         | docID gaps (varbyte, first relative to 0) | freqs (varbyte)
```

All integers in a record are varbyte. Records are in byte order of the term.
DocIDs and frequencies are stored in separate blocks, not interleaved.
`last_doc` lets the merger re-base the first gap of the next run's record.

### Page table (`src/doctable.rs`)

The page table uses fixed 16-byte records, so record *d* sits at byte
`16*d`. Each record holds:

- `pid` (u32)
- `len` (u32), the number of terms, used by BM25
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
of the Rust document table for docID-to-PID lookup. See
[the final index format](internal/finalindex/FORMAT.md) for the byte layout
and the `SeekGE`/`Document` reader APIs.

## Query CLI and package layout

```sh
go run ./cmd/query
```

- `cmd/query/main.go`: CLI input, startup wiring, and result printing.
- `internal/query/query.go`: `Evaluate` entry point, term preparation, and PID resolution.
- `internal/query/evaluation.go`: posting iterators, AND/OR DAAT evaluation,
  the top-ten heap, and the BM25 scoring placeholder.
- `internal/finalindex/lexicon.go`: `LoadLexicon`, called once at startup.
- `internal/finalindex/reader.go`: binary index reads and decompression.

BM25 scores are still placeholders; query tokenization currently lowercases
and splits on whitespace. The existing compressed index format is unchanged.
