//! Reads the MS MARCO passage collection (`pid<TAB>passage` per line), assigns
//! docIDs 0, 1, 2, ... in file order, tokenizes each passage, and writes:
//!
//! * `<dir>/runs/postings-NNNNNN.bin`  sorted runs of (term, docID, freq)
//!   postings in the compressed binary format of `index::runfile`
//!   (or `.tsv` text with `--ascii`, for debugging);
//! * `<dir>/pagetable.bin`  docID -> (pid, length in terms, byte offset of
//!   the passage in the collection file), see `index::doctable`;
//! * `<dir>/meta.txt`  collection statistics (number of docs, average
//!   length, number of runs, ...) needed by the merger and by BM25.
//!
//! Algorithm:
//!
//! 1. For each passage, tokenize and sort its terms locally to count each
//!    term's frequency, producing one (term, docID, freq) posting per
//!    distinct term.
//! 2. Append the postings to a large preallocated buffer in RAM: a flat byte
//!    arena of term strings plus an array of fixed-size posting records.
//! 3. When the buffer is full, hand it to a background thread that sorts it by
//!    (term, docID) and writes it out as one run, while the main thread keeps
//!    parsing into a second buffer. Memory use is therefore bounded by two
//!    buffers (`--mem-mb` in total), independent of the collection size.

use index::doctable::{DocRecord, DocTableWriter};
use index::runfile::{encode_postings, RunWriter};
use index::tokenizer::{Tokenizer, MAX_TERM_LEN};
use std::error::Error;
use std::fs::{self, File};
use std::io::{self, BufRead, BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::thread;
use std::time::Instant;

struct Args {
    input: PathBuf,
    dir: PathBuf,
    limit: u64,
    show: bool,
    mem_mb: usize,
    ascii: bool,
}

const USAGE: &str = "\
usage: parse [options]
  --input PATH   unpacked passage collection (default data/collection.tsv)
  --dir PATH     output directory (default data)
  --mem-mb N     RAM for posting buffers in MB, split over two buffers (default 1024)
  --limit N      parse only the first N passages (0 = all)
  --show         print every posting (use with a small --limit)
  --ascii        write runs as readable .tsv instead of binary (debugging)";

fn parse_args() -> Result<Args, String> {
    let mut a = Args {
        input: "data/collection.tsv".into(),
        dir: "data".into(),
        limit: 0,
        show: false,
        mem_mb: 1024,
        ascii: false,
    };
    let mut it = std::env::args().skip(1);
    while let Some(flag) = it.next() {
        // Accept both --flag and -flag (like Go's flag package).
        let name = flag.trim_start_matches('-');
        let mut value = || it.next().ok_or_else(|| format!("missing value for {flag}"));
        match name {
            "input" => a.input = value()?.into(),
            "dir" => a.dir = value()?.into(),
            "limit" => a.limit = value()?.parse().map_err(|_| "bad --limit")?,
            "mem-mb" => a.mem_mb = value()?.parse().map_err(|_| "bad --mem-mb")?,
            "show" => a.show = true,
            "ascii" => a.ascii = true,
            "h" | "help" => return Err(USAGE.into()),
            _ => return Err(format!("unknown option {flag}\n{USAGE}")),
        }
    }
    if a.mem_mb < 4 {
        return Err("--mem-mb must be at least 4".into());
    }
    Ok(a)
}

// posting buffer
/// One intermediate posting. The term itself lives in the buffer's arena.
/// `prefix` holds its first 8 bytes (big-endian, zero-padded) reducing cache
/// misses.
#[derive(Clone, Copy)]
struct Post {
    prefix: u64,
    doc: u32,
    off: u32,
    freq: u32,
    len: u8,
}

const POST_BYTES: usize = std::mem::size_of::<Post>(); // 24

/// Preallocated buffer of postings. never grows, so memory use is fixed.
struct Buffer {
    posts: Vec<Post>,
    arena: Vec<u8>,
}

impl Buffer {
    /// Split `bytes` between posting records (3/4) and term bytes (1/4):
    /// MS MARCO terms average well under 8 bytes vs. 24 per record.
    fn with_budget(bytes: usize) -> Self {
        let n_posts = bytes * 3 / 4 / POST_BYTES;
        let arena = (bytes - n_posts * POST_BYTES).min(u32::MAX as usize);
        Self { posts: Vec::with_capacity(n_posts), arena: Vec::with_capacity(arena) }
    }

    fn fits(&self, n_posts: usize, n_bytes: usize) -> bool {
        self.posts.len() + n_posts <= self.posts.capacity()
            && self.arena.len() + n_bytes <= self.arena.capacity()
    }

    #[inline]
    fn push(&mut self, term: &[u8], prefix: u64, doc: u32, freq: u32) {
        let off = self.arena.len() as u32;
        self.arena.extend_from_slice(term);
        self.posts.push(Post { prefix, doc, off, freq, len: term.len() as u8 });
    }

    fn clear(&mut self) {
        self.posts.clear();
        self.arena.clear();
    }
}

#[inline]
fn prefix_of(t: &[u8]) -> u64 {
    let mut b = [0u8; 8];
    let n = t.len().min(8);
    b[..n].copy_from_slice(&t[..n]);
    u64::from_be_bytes(b)
}

#[inline]
fn term_of<'a>(arena: &'a [u8], p: &Post) -> &'a [u8] {
    &arena[p.off as usize..p.off as usize + p.len as usize]
}

/// Compare two terms given their 8-byte prefixes. Terms contain no zero
/// bytes, so the zero-padded big-endian prefix order agrees with byte-wise
/// term order, and equal prefixes of two short (<= 8 byte) terms mean equal
/// terms. Only long terms with equal prefixes need a look at the remaining bytes.
#[inline]
fn cmp_terms(pa: u64, a: &[u8], pb: u64, b: &[u8]) -> std::cmp::Ordering {
    pa.cmp(&pb).then_with(|| {
        if a.len() <= 8 && b.len() <= 8 {
            a.len().cmp(&b.len())
        } else {
            a[a.len().min(8)..].cmp(&b[b.len().min(8)..])
        }
    })
}

/// Sort a full buffer by (term bytes, docID).
fn sort_buffer(buf: &mut Buffer) {
    let Buffer { posts, arena } = buf;
    posts.sort_unstable_by(|a, b| {
        cmp_terms(a.prefix, term_of(arena, a), b.prefix, term_of(arena, b)).then(a.doc.cmp(&b.doc))
    });
}

#[derive(Default)]
struct RunStats {
    runs: usize,
    terms: u64,
    postings: u64,
    bytes: u64,
}

/// Write a sorted buffer as one run: one record per distinct term.
fn write_run(buf: &Buffer, path: &Path, ascii: bool) -> io::Result<(u64, u64)> {
    let (posts, arena) = (&buf.posts, &buf.arena);
    let mut ascii_out = if ascii {
        Some(BufWriter::with_capacity(4 << 20, File::create(path)?))
    } else {
        None
    };
    let mut bin_out = if ascii { None } else { Some(RunWriter::create(path)?) };
    let (mut docs, mut freqs) = (Vec::new(), Vec::new());
    let mut terms = 0u64;

    let mut i = 0;
    while i < posts.len() {
        let term = term_of(arena, &posts[i]);
        let mut j = i + 1;
        while j < posts.len() && posts[j].prefix == posts[i].prefix && term_of(arena, &posts[j]) == term {
            j += 1;
        }
        let group = &posts[i..j];
        terms += 1;
        if let Some(w) = bin_out.as_mut() {
            let last = encode_postings(group.iter().map(|p| (p.doc, p.freq)), &mut docs, &mut freqs);
            w.write_record(term, group.len() as u32, last, &docs, &freqs)?;
        } else if let Some(w) = ascii_out.as_mut() {
            // term<TAB>count<TAB>doc:freq doc:freq ...
            w.write_all(term)?;
            write!(w, "\t{}\t", group.len())?;
            for (k, p) in group.iter().enumerate() {
                if k > 0 {
                    w.write_all(b" ")?;
                }
                write!(w, "{}:{}", p.doc, p.freq)?;
            }
            w.write_all(b"\n")?;
        }
        i = j;
    }

    if let Some(w) = bin_out {
        let (_, bytes) = w.finish()?;
        Ok((terms, bytes))
    } else {
        let mut w = ascii_out.unwrap();
        w.flush()?;
        Ok((terms, w.get_ref().metadata()?.len()))
    }
}

/// Background thread: sorts and writes full buffers, then returns them empty
/// so the main thread can reuse the allocation.
fn spawn_writer(
    runs_dir: PathBuf,
    ascii: bool,
    full_rx: Receiver<(usize, Buffer)>,
    free_tx: SyncSender<Buffer>,
) -> thread::JoinHandle<io::Result<RunStats>> {
    thread::spawn(move || {
        let mut stats = RunStats::default();
        for (run, mut buf) in full_rx {
            let t = Instant::now();
            sort_buffer(&mut buf);
            let sorted = t.elapsed().as_secs_f64();
            let ext = if ascii { "tsv" } else { "bin" };
            let path = runs_dir.join(format!("postings-{run:06}.{ext}"));
            let (terms, bytes) = write_run(&buf, &path, ascii)?;
            eprintln!(
                "  run {run}: {} postings, {} terms, {:.1} MB (sort {:.1}s, write {:.1}s)",
                buf.posts.len(),
                terms,
                bytes as f64 / 1e6,
                sorted,
                t.elapsed().as_secs_f64() - sorted
            );
            stats.runs += 1;
            stats.terms += terms;
            stats.postings += buf.posts.len() as u64;
            stats.bytes += bytes;
            buf.clear();
            let _ = free_tx.send(buf); // fails only after the main thread is done
        }
        Ok(stats)
    })
}

/// Swap the full buffer for an empty one and queue it for writing.
fn hand_off(
    buf: &mut Buffer,
    run: &mut usize,
    full_tx: &SyncSender<(usize, Buffer)>,
    free_rx: &Receiver<Buffer>,
) -> bool {
    if buf.posts.is_empty() {
        return true;
    }
    let Ok(empty) = free_rx.recv() else { return false };
    let full = std::mem::replace(buf, empty);
    if full_tx.send((*run, full)).is_err() {
        return false;
    }
    *run += 1;
    true
}

// Main parse loop
fn main() {
    let args = match parse_args() {
        Ok(a) => a,
        Err(msg) => {
            eprintln!("{msg}");
            std::process::exit(2);
        }
    };
    if let Err(e) = run(&args) {
        eprintln!("parse: {e}");
        std::process::exit(1);
    }
}

fn run(args: &Args) -> Result<(), Box<dyn Error>> {
    let runs_dir = args.dir.join("runs");
    fs::create_dir_all(&runs_dir)?;
    // Remove runs left over from an earlier invocation so the merger never
    // picks up stale files.
    for entry in fs::read_dir(&runs_dir)? {
        let p = entry?.path();
        let name = p.file_name().and_then(|n| n.to_str()).unwrap_or("");
        if name.starts_with("postings-") && (name.ends_with(".bin") || name.ends_with(".tsv")) {
            fs::remove_file(&p)?;
        }
    }

    let input = File::open(&args.input)
        .map_err(|e| format!("cannot open {}: {e}", args.input.display()))?;
    let input_bytes = input.metadata()?.len();
    let mut reader = BufReader::with_capacity(8 << 20, input);
    let mut pagetable = DocTableWriter::create(&args.dir.join("pagetable.bin"))?;

    // Two buffers: one being filled here, one being sorted/written in the background.
    let per_buffer = args.mem_mb * (1 << 20) / 2;
    let (full_tx, full_rx) = sync_channel::<(usize, Buffer)>(1);
    let (free_tx, free_rx) = sync_channel::<Buffer>(2);
    free_tx.send(Buffer::with_budget(per_buffer))?;
    let writer = spawn_writer(runs_dir.clone(), args.ascii, full_rx, free_tx);
    let mut buf = Buffer::with_budget(per_buffer);
    let mut run = 0usize;

    println!(
        "parsing {} ({:.2} GB) with {} MB of posting buffers",
        args.input.display(),
        input_bytes as f64 / 1e9,
        args.mem_mb
    );
    let start = Instant::now();

    let mut tokenizer = Tokenizer::new();
    let mut line = Vec::with_capacity(1 << 16);
    let mut doc_terms: Vec<u8> = Vec::with_capacity(1 << 16); // this doc's terms, concatenated
    let mut spans: Vec<(u64, u32, u8)> = Vec::with_capacity(4096); // (prefix, offset, len) into doc_terms
    let (mut docs, mut tokens, mut postings) = (0u64, 0u64, 0u64);
    let (mut offset, mut malformed, mut bad_pids) = (0u64, 0u64, 0u64);
    let mut writer_failed = false;
    let stdout = io::stdout();
    let mut show_out = BufWriter::new(stdout.lock());

    loop {
        line.clear();
        let n = reader.read_until(b'\n', &mut line)?;
        if n == 0 {
            break;
        }
        let line_offset = offset;
        offset += n as u64;

        let mut l = &line[..];
        while let [rest @ .., b'\n' | b'\r'] = l {
            l = rest;
        }
        let Some(tab) = l.iter().position(|&b| b == b'\t') else {
            malformed += 1; // no tab: not a passage line; does not get a docID
            continue;
        };
        let (pid_bytes, text) = (&l[..tab], &l[tab + 1..]);
        let pid = std::str::from_utf8(pid_bytes)
            .ok()
            .and_then(|s| s.trim().parse::<u32>().ok())
            .unwrap_or_else(|| {
                bad_pids += 1;
                u32::MAX
            });
        let doc = docs as u32;

        // 1. Tokenize into a per-document scratch area.
        doc_terms.clear();
        spans.clear();
        tokenizer.tokenize(text, |t| {
            spans.push((prefix_of(t), doc_terms.len() as u32, t.len() as u8));
            doc_terms.extend_from_slice(t);
        });
        let length = spans.len() as u32;

        // 2. Sort this document's terms so equal terms are adjacent.
        let span = |&(_, o, n): &(u64, u32, u8)| &doc_terms[o as usize..o as usize + n as usize];
        spans.sort_unstable_by(|a, b| cmp_terms(a.0, span(a), b.0, span(b)));

        // 3. Make room (upper bound: every token distinct), then append one
        //    (term, doc, freq) posting per distinct term.
        if !buf.fits(spans.len(), doc_terms.len()) {
            if !hand_off(&mut buf, &mut run, &full_tx, &free_rx) {
                writer_failed = true;
                break;
            }
            if !buf.fits(spans.len(), doc_terms.len()) {
                // A single passage larger than a whole buffer: allow growth.
                buf.posts.reserve(spans.len());
                buf.arena.reserve(doc_terms.len());
            }
        }
        let mut i = 0;
        while i < spans.len() {
            let (prefix, term) = (spans[i].0, span(&spans[i]));
            let mut j = i + 1;
            while j < spans.len() && spans[j].0 == prefix && span(&spans[j]) == term {
                j += 1;
            }
            let freq = (j - i) as u32;
            buf.push(term, prefix, doc, freq);
            if args.show {
                writeln!(show_out, "{:<width$} {doc} {freq}", String::from_utf8_lossy(term), width = MAX_TERM_LEN)?;
            }
            postings += 1;
            i = j;
        }

        pagetable.push(DocRecord { pid, len: length, offset: line_offset })?;
        docs += 1;
        tokens += length as u64;
        if docs % 1_000_000 == 0 {
            let secs = start.elapsed().as_secs_f64();
            println!(
                "  parsed {docs} passages ({:.0}% of input, {:.0} MB/s, {run} runs so far)",
                100.0 * offset as f64 / input_bytes.max(1) as f64,
                offset as f64 / 1e6 / secs
            );
        }
        if args.limit > 0 && docs >= args.limit {
            break;
        }
    }
    show_out.flush()?;
    drop(show_out);

    // Flush the final, partially filled buffer, then wait for the writer.
    if !writer_failed && !hand_off(&mut buf, &mut run, &full_tx, &free_rx) {
        writer_failed = true;
    }
    drop(full_tx);
    let stats = writer.join().map_err(|_| "writer thread panicked")??;
    if writer_failed {
        return Err("writer thread stopped unexpectedly".into());
    }
    pagetable.finish()?;
    let elapsed = start.elapsed().as_secs_f64();

    let avg = if docs > 0 { tokens as f64 / docs as f64 } else { 0.0 };
    let meta = format!(
        "format=IRN1\n\
         input={}\n\
         num_docs={docs}\n\
         total_terms={tokens}\n\
         avg_doc_len={avg:.6}\n\
         num_postings={postings}\n\
         num_runs={}\n\
         run_ext={}\n\
         run_terms_total={}\n\
         run_bytes_total={}\n\
         malformed_lines={malformed}\n\
         non_numeric_pids={bad_pids}\n\
         max_term_len={MAX_TERM_LEN}\n\
         parse_seconds={elapsed:.1}\n",
        args.input.display(),
        stats.runs,
        if args.ascii { "tsv" } else { "bin" },
        stats.terms,
        stats.bytes,
    );
    fs::write(args.dir.join("meta.txt"), meta)?;
    debug_assert_eq!(stats.postings, postings);

    println!(
        "done: {docs} passages, {tokens} term occurrences, {postings} postings, avg length {avg:.1}, {elapsed:.1}s"
    );
    println!(
        "      {} runs, {:.1} MB of runs in {}, page table {:.1} MB",
        stats.runs,
        stats.bytes as f64 / 1e6,
        runs_dir.display(),
        docs as f64 * 16.0 / 1e6
    );
    if malformed > 0 || bad_pids > 0 {
        println!("      skipped {malformed} lines without a tab; {bad_pids} non-numeric pids");
    }
    Ok(())
}
