# Index lookup overview

```text
Query word → lexicon → block descriptors → compressed posting blocks
```

| Stage | Data it contains | Format |
|---|---|---|
| **Query word** | `"cat"` | A string |
| **Lexicon** (`lexicon.tsv`) | Term, document frequency, offset of its first block descriptor, number of blocks | Tab-separated text, one row per term |
| **Block descriptors** (`blocks.bin`) | Last docID, posting count, payload offset, document-byte length, frequency-byte length | Fixed **24-byte binary records**, one per block |
| **Posting blocks** (`postings.bin`) | Document IDs and their term frequencies | Two separate **varbyte-compressed** sections per block |

## Example lookup

This example uses two postings per block. The real index defaults to 128.
`X`, `A`, and `B` represent byte offsets in their respective files.

```text
"cat"
  ↓
Lexicon: cat occurs in 4 documents; descriptors start at X; 2 blocks
  ↓
Descriptors:
  Block 0: ends at docID 127; posting bytes start at A
  Block 1: ends at docID 300; posting bytes start at B
  ↓
Decoded postings:
  Block 0: [(0, frequency 1), (127, frequency 300)]
  Block 1: [(128, frequency 2), (300, frequency 4)]
```

Within each posting block, the first docID is absolute; subsequent docIDs
are stored as gaps. Frequencies are stored directly. Both use unsigned
LEB128-style varbyte encoding.

After finding a result, **`pagetable.bin`** maps its internal docID back to
the original PID, document length, and passage location. Each record is
16 bytes and starts at byte offset `16 × docID`.

The generated index files are in `data/final-index/`.
