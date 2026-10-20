# Indexing/Processing Write-Up

## End-to-end approach

The system transforms a text collection into a searchable inverted index in
three phases: Rust parses the collection and writes sorted intermediate runs;
Go merges those runs into a final block-compressed index; and the Go query
processor searches that index using conjunctive (AND) or disjunctive (OR)
evaluation and BM25 ranking. The query processor returns up to ten results,
each containing the original passage ID, internal document ID, and score.

### Source collection: `collection.tsv`

The input is the MS MARCO passage collection stored in `data/collection.tsv`.
Each line contains a passage identifier (PID), a tab, and the passage text:

```text
PID<TAB>passage text<NEWLINE>
```

For example, with actual tabs between the columns:

```text
42	Cat cat dog
99	Dog mouse
```

The PID identifies the passage in the source dataset. The parser assigns a
separate internal docID to each valid passage line, beginning at zero and
increasing in input order. Thus, the example receives docIDs 0 and 1 even
though its PIDs are 42 and 99. Lines without a tab are skipped.

Tokenization and frequency counting turn these passages into postings:

| Term | Postings `(docID, frequency)` |
|---|---|
| `cat` | `(0, 2)` |
| `dog` | `(0, 1), (1, 1)` |
| `mouse` | `(1, 1)` |

A posting records one term's occurrence count in one document. Document length
counts all accepted token occurrences, including repetitions: the two example
documents have lengths 3 and 2. The index stores frequencies rather than word
positions or precomputed impact scores.

### Phase 1: Rust parsing and intermediate files

The parser processes the collection incrementally rather than holding all
postings in memory. It counts term frequencies within each document, collects
postings in reusable buffers, and writes each sorted buffer as a run grouped
by term. Two buffers allow parsing to overlap with background sorting and
writing. Their initial allocation is controlled by `--mem-mb`; this is not a
strict total-memory limit, since unusually large passages can grow a buffer
and other scratch and I/O buffers use additional memory.

The paths below are relative to the parser's selected output directory, such
as `data/rust-full-run/`.

| File | Purpose | Format and main fields |
|---|---|---|
| `runs/postings-NNNNNN.bin` | Sorted partial inverted lists to be consumed by the Go merger | Binary IRN1 format. A 12-byte header contains the four ASCII bytes `IRN1` and an eight-byte little-endian term-record count. Each record contains `term_len`, term bytes, posting `count`, absolute `last_doc`, `docs_len`, `freqs_len`, compressed document gaps, and compressed frequencies. All record integers use unsigned varbyte encoding. Terms are in byte order, and postings within each term are in increasing docID order |
| `pagetable.bin` | Map internal docIDs to source passage information | Binary, with no header. Each 16-byte record contains PID (`u32`), accepted token count (`u32`), and source-line byte offset (`u64`), all little-endian. Document `d` is at byte offset `16 × d` |
| `meta.txt` | Describe the collection and parser output | Text, one `key=value` per line. Includes `format=IRN1`, source path, `num_docs`, `total_terms`, `avg_doc_len`, `num_postings`, `num_runs`, run sizes, diagnostic counts, and parse time |

Each document stays entirely in one run, so later runs contain larger docIDs.
A term can appear in several runs; its records are partial lists that must
be combined. Consequently, the sum of term-record counts across runs can be
larger than the final number of unique terms.

Document IDs are gap-encoded within each term record: the first gap is relative
to zero, and later gaps are relative to the preceding docID. Frequencies are
stored directly in a separate compressed section. Varbyte uses fewer bytes
for small integers; fixed-width little-endian fields provide predictable
locations for metadata. The optional `--ascii` mode writes readable TSV runs
for inspection, but the Go merger consumes the default binary runs.

### Phase 2: Go merging and the final index

The merger opens the Rust runs and uses a min-heap ordered by term, then run
number. It gathers all records for the current term in run order. Since run
document ranges increase without overlap, those records can be joined without
sorting individual docIDs again or summing duplicate postings. The merger
decodes source postings, validates them, and re-encodes them into independently
decodable blocks of 128 postings by default. The last block of a term may be
smaller, and a block can span input-run boundaries.

The final index consists of the following files, relative to the merger's
selected output directory, such as `data/final-index/`:

| File | Purpose | Format and main fields |
|---|---|---|
| `postings.bin` | Store the final compressed posting payloads | Binary, beginning with the four ASCII bytes `FIP1`. Each block contains a varbyte document-gap section followed by a varbyte frequency section. The first docID in each block is absolute, so decoding can begin at any block |
| `blocks.bin` | Locate posting blocks and support skipping by docID | Binary, beginning with `FIB1`. Each descriptor occupies 24 bytes and contains five fixed-width integer fields, each encoded in little-endian order: last docID (`u32`, 4 bytes), posting count (`u32`, 4 bytes), payload offset in `postings.bin` (`u64`, 8 bytes), document-section byte length (`u32`, 4 bytes), and frequency-section byte length (`u32`, 4 bytes). A term's descriptors are consecutive |
| `lexicon.tsv` | Find the descriptors for a query term | Tab-separated text with header `term`, `doc_frequency`, `blocks_offset`, `block_count`. Contains one row per unique term in byte order. `blocks_offset` points into `blocks.bin`, not `postings.bin` |
| `pagetable.bin` | Resolve result PIDs and retrieve lengths for BM25 | Byte-for-byte copy of the Rust 16-byte-per-document table |
| `collection-meta.txt` | Supply collection statistics for scoring | Unchanged text copy of Rust's `meta.txt`; includes `num_docs` and `avg_doc_len` |
| `meta.txt` | Summarize the completed final index | Text `key=value` fields: `format=FIP1`, `num_docs`, `num_terms`, `num_postings`, `num_blocks`, and `block_size` |

The merger also creates a temporary `INCOMPLETE` marker while building the
index. It removes the marker only after successful completion; readers reject
an index that still contains it. The output directory must not already exist.

### Phase 3: Query processing and file navigation

At startup, the query program opens the three binary files and loads the
lexicon and collection statistics into memory. For each distinct query term,
`OpenList` looks up its lexicon entry and reads its block descriptors without
loading or decompressing posting payloads. `NextGEQ(target)` uses the blocks'
last docIDs to skip unnecessary blocks and decodes only the required docID
chunk. `GetFreq()` reads and decodes the corresponding frequency chunk only
when scoring requests it. Each cursor retains at most one decoded block's
docIDs and frequencies, plus its term's descriptors.

For AND queries, lists are ordered shortest first and advanced until all
cursors agree on a document. For OR queries, current docIDs are merged so
every document present in any list becomes a candidate. BM25 uses matching
term frequencies, lexicon document frequencies, collection statistics, and
the candidate's length from `pagetable.bin`. A heap retains the ten best
results, with fewer returned when fewer documents match. PIDs are resolved
for the retained results. The CLI also caches successful query results for
the session, keyed by normalized query text and mode.

The relationships between the stages and files can be summarized as:

```text
collection.tsv
    │
    ▼ Rust parse
sorted IRN1 runs + pagetable.bin + meta.txt
    │
    ▼ Go merge
lexicon.tsv + blocks.bin + postings.bin
            + pagetable.bin + collection-meta.txt + meta.txt
    │
    ▼ Go query
query term → lexicon entry → block descriptor → compressed posting chunk
                                                   │
                              BM25 ← frequency + docID + document length
                                                   │
                                                   ▼
                                      up to ten ranked results
```

The original passage text remains in `collection.tsv`; it is not copied into
the index. Document-table offsets allow later retrieval of passage text, but
the current CLI prints identifiers and scores without fetching snippets.
Query preparation currently lowercases and splits on whitespace, whereas the
Rust indexing tokenizer also splits ASCII punctuation and drops tokens
containing non-ASCII bytes or exceeding 64 bytes. This difference is a current
limitation rather than a property of the binary index format.

## Rust implementation details

1. Overview
The parse program reads the MS MARCO passage collection (pid<TAB>passage per line), assigns docIDs in file order, tokenizes each passage, and writes sorted, compressed intermediate runs of postings plus a page table. A later merge phase combines the runs into the final inverted index.
It implements the Merge-Sort Improved algorithm: postings are collected in a fixed-size RAM buffer, and each full buffer is sorted and written to disk as one run. Memory use is therefore bounded by a command-line setting, independent of collection size. No external crates are used.

2. Code structure
File
Role
src/bin/parse.rs
Main program: reads the collection, builds the posting buffer, sorts and writes runs, writes the page table and meta.txt.
src/tokenizer.rs
Turns passage bytes into lowercase terms. Shared with the future query processor.
src/varbyte.rs
Variable-byte integer encoding and decoding.
src/runfile.rs
Run file format: RunWriter, RunReader, encode_postings.
src/doctable.rs
Page table records (docID → pid, length, file offset).
src/bin/dumprun.rs
Debug tool: prints run contents or validates them with --check.


3. Processing pipeline
Read and assign docIDs. Lines are read with a buffered reader. Each line with a tab gets the next docID (0, 1, 2, …); lines without a tab are counted as malformed and skipped. Windows line endings are handled.
Tokenize the passage into a per-document scratch area (see Section 4).
Count frequencies locally. The document's terms are sorted so repeated terms are adjacent, producing one (term, docID, freq) posting per distinct term instead of one per occurrence.
Buffer postings. Each posting is appended to a preallocated buffer: an array of 24-byte records (8-byte term prefix, docID, freq, arena offset, length) plus a byte arena holding the term strings. The buffer never grows.
Sort and write in the background. When the buffer is full, it is handed to a writer thread that sorts it by (term, docID) and writes it as one run. Meanwhile the main thread parses into a second buffer, overlapping sorting with parsing.
Finish. The last partial buffer is written, then the page table and meta.txt (document count, total and average length, number of runs) are flushed.

Sorting compares the 8-byte big-endian term prefixes first, so most comparisons never touch the arena. Because docIDs only increase, every docID in run k+1 is larger than every docID in run k; the merger can therefore concatenate a term's records across runs without re-sorting docIDs.
4. Tokenization
Terms are delimited by whitespace only (space, tab, newline; other ASCII control characters are treated as whitespace).
Inside a term, ASCII letters and digits are kept and lowercased; punctuation is removed rather than splitting the term.
A token containing any non-ASCII byte is dropped whole, so foreign text and malformed UTF-8 are ignored safely.
Terms longer than 64 bytes (after removing punctuation) are dropped. No stemming or stopword removal.
Implementation: a 256-entry byte-class table built at compile time maps each byte to SEP (ends the token), SKIP (removed), BAD (poisons the token) or its lowercase character. The loop runs on raw bytes with no allocation.
Input
Terms
don't panic!
dont, panic
The U.S. economy grew 2.3%
the, us, economy, grew, 23
e-mail COVID-19
email, covid19
Café au lait
au, lait


5. Output files
Run files (data/runs/postings-NNNNNN.bin)
Each run is a header followed by one record per distinct term, in byte order of the term. All record integers are varbyte encoded.
header:  "IRN1" | num_terms (u64, little-endian)
record:  term_len | term | count | last_doc | docs_len | freqs_len
         | docID gaps (first gap relative to 0) | frequencies
DocIDs and frequencies are stored in separate blocks, as in the final index. last_doc lets the merger re-base the first gap of the next run's record without decoding the block. With --ascii, runs are written as readable text (term<TAB>count<TAB>doc:freq …).
Page table (data/pagetable.bin)
Fixed 16-byte little-endian records in docID order, so record d is at byte offset 16·d:
Field
Type
Meaning
pid
u32
Original MS MARCO passage id
len
u32
Document length in terms (needed by BM25)
offset
u64
Byte offset of the passage line in the collection, for snippets


Metadata (data/meta.txt)
Key-value text: num_docs, total_terms, avg_doc_len, num_postings, num_runs, counts of malformed lines, and parse time.
6. Usage
cargo build --release
./target/release/parse --input data/collection.tsv --dir data --mem-mb 1024
./target/release/dumprun --check data/runs/*.bin
Flag
Default
Meaning
--input
data/collection.tsv
Collection file
--dir
data
Output directory
--mem-mb
1024
RAM for the two posting buffers
--limit
0 (all)
Parse only the first N passages
--show
off
Print every posting
--ascii
off
Write text runs instead of binary


