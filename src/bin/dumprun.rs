//! dumprun: inspect and validate run files written by `parse`.
//!
//!   dumprun data/runs/postings-000000.bin              print records as text
//!   dumprun --limit 20 data/runs/postings-000000.bin   first 20 terms only
//!   dumprun --term zebra data/runs/*.bin               one term across runs
//!   dumprun --check data/runs/*.bin                    validate, print summary
//!
//! `--check` verifies, for the files in the order given: terms strictly
//! increasing within each run; docIDs strictly increasing within each record;
//! frequencies >= 1; `count` and `last_doc` consistent with the blocks; no
//! trailing bytes; and every docID in a run larger than all docIDs in earlier
//! runs (the property the merger relies on).

use index::runfile::{RunReader, RunRecord};
use std::io::{self, BufWriter, Write};
use std::path::PathBuf;

fn main() {
    if let Err(e) = run() {
        eprintln!("dumprun: {e}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), Box<dyn std::error::Error>> {
    let (mut check, mut limit, mut term) = (false, u64::MAX, None::<Vec<u8>>);
    let mut files = Vec::new();
    let mut it = std::env::args().skip(1);
    while let Some(a) = it.next() {
        match a.as_str() {
            "--check" => check = true,
            "--limit" => limit = it.next().ok_or("missing --limit value")?.parse()?,
            "--term" => term = Some(it.next().ok_or("missing --term value")?.into_bytes()),
            _ if a.starts_with('-') => return Err(format!("unknown option {a}").into()),
            _ => files.push(PathBuf::from(a)),
        }
    }
    if files.is_empty() {
        return Err("usage: dumprun [--check] [--limit N] [--term T] RUN_FILE...".into());
    }

    let out = io::stdout();
    let mut out = BufWriter::new(out.lock());
    let mut rec = RunRecord::default();
    let mut prev_runs_max: Option<u32> = None;
    let (mut all_terms, mut all_postings) = (0u64, 0u64);

    for path in &files {
        let mut r = RunReader::open(path)?;
        let (mut prev_term, mut n, mut postings) = (Vec::<u8>::new(), 0u64, 0u64);
        let (mut run_min, mut run_max) = (u32::MAX, 0u32);
        while r.next_into(&mut rec)? {
            let ps = rec.postings();
            if check {
                let where_ = || format!("{} term #{n} {:?}", path.display(), String::from_utf8_lossy(&rec.term));
                if n > 0 && rec.term <= prev_term {
                    return Err(format!("{}: terms out of order", where_()).into());
                }
                if ps.is_empty() || ps.len() != rec.count as usize {
                    return Err(format!("{}: bad count", where_()).into());
                }
                if ps.windows(2).any(|w| w[0].0 >= w[1].0) {
                    return Err(format!("{}: docIDs not strictly increasing", where_()).into());
                }
                if ps.iter().any(|p| p.1 == 0) {
                    return Err(format!("{}: zero frequency", where_()).into());
                }
                if ps.last().unwrap().0 != rec.last_doc {
                    return Err(format!("{}: last_doc mismatch", where_()).into());
                }
                if let Some(m) = prev_runs_max {
                    if ps[0].0 <= m {
                        return Err(format!("{}: docID {} overlaps an earlier run", where_(), ps[0].0).into());
                    }
                }
                prev_term.clear();
                prev_term.extend_from_slice(&rec.term);
                run_min = run_min.min(ps[0].0);
                run_max = run_max.max(rec.last_doc);
            } else if term.as_ref().map_or(n < limit, |t| *t == rec.term) {
                out.write_all(&rec.term)?;
                write!(out, "\t{}\t", rec.count)?;
                for (k, (d, f)) in ps.iter().enumerate() {
                    write!(out, "{}{d}:{f}", if k > 0 { " " } else { "" })?;
                }
                writeln!(out)?;
            }
            n += 1;
            postings += ps.len() as u64;
        }
        if n != r.num_terms {
            return Err(format!("{}: header says {} terms, read {n}", path.display(), r.num_terms).into());
        }
        if !r.at_eof()? {
            return Err(format!("{}: trailing bytes after last record", path.display()).into());
        }
        if check {
            writeln!(out, "ok  {}: {n} terms, {postings} postings, docIDs {run_min}..={run_max}", path.display())?;
            prev_runs_max = Some(run_max);
        }
        all_terms += n;
        all_postings += postings;
    }
    if check {
        writeln!(out, "all {} runs ok: {all_terms} term records, {all_postings} postings", files.len())?;
    }
    Ok(())
}
