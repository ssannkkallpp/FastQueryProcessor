# FastQueryProcessor

## Build and run

Place unpacked collection from source tar at `data/collection.tsv`, then run:

```sh
go build -o parser main.go
./parser -input data/collection.tsv -dir data/full-run
```

Results are generated into `data/full-run/`:

- `pagetable.tsv`: document ID, original passage ID, and term count/length.
- `postings.tsv`: term and document ID, sorted within each chunk.

