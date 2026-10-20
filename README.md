# Build and run

Run from the repository root with Rust/Cargo and Go installed. Place the source
collection at `data/collection.tsv`.

## Build the three binaries

```sh
cargo build --release --bin parse
go build -o /tmp/search-merge ./cmd/merge
go build -o /tmp/search-query ./cmd/query
```

## Run all three phases

Create a fresh output directory, parse the collection, merge, then query:

```sh
run_dir=$(mktemp -d "$PWD/data/run")
./target/release/parse --input data/collection.tsv --dir "$run_dir/rust" --mem-mb 1024
/tmp/search-merge -input "$run_dir/rust" -dir "$run_dir/index"
/tmp/search-query -dir "$run_dir/index"
```

Enter a query, then choose `conjunctive` or `disjunctive`. Enter `:quit` to exit.

To query an existing index instead:

```sh
/tmp/search-query -dir data/final-index
```
