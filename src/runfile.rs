//! Intermediate "run" files: the output of `parse`, the input of `merge`.
//!
//!`parse` fills a fixed-size RAM buffer with (term, docID, freq) postings.
//! When the buffer is full it is sorted by (term, docID) and written out as
//! one run, grouped by term
//!
//! Because documents are parsed in order, every docID in run k+1 is larger
//! than every docID in run k. The merger can therefore build a term's full
//! inverted list by concatenating that term's records from run 0, 1, 2, ...:
//! it only has to k-way merge on terms, never re-sort docIDs.
//!
//! ## Binary layout (integers are varbyte unless noted)
//!
//! ```text
//! header:   magic "IRN1" (4 bytes) | num_terms (u64 little-endian)
//! record*:  term_len | term bytes
//!           | count          number of postings in this record
//!           | last_doc       largest docID in this record (absolute)
//!           | docs_len       byte length of the docID block
//!           | freqs_len      byte length of the frequency block
//!           | docID block    `count` varbyte gaps; the first gap is relative to 0
//!           | freq block     `count` varbyte term frequencies (>= 1)
//! ```
//!
//! Records are sorted by term in plain byte order, which the merger must use
//! too. DocIDs and frequencies sit in separate blocks (not interleaved), like
//! the final index. `last_doc` lets the merger re-base the first gap of the
//! next run's record without decoding this one.

use crate::varbyte;
use std::fs::File;
use std::io::{self, BufRead, BufReader, BufWriter, Read, Seek, SeekFrom, Write};
use std::path::Path;

pub const MAGIC: &[u8; 4] = b"IRN1";

/// Gap-encode one term's postings (sorted by docID) into separate blocks.
/// Returns the last docID. `docs` and `freqs` are cleared first.
pub fn encode_postings(
    postings: impl IntoIterator<Item = (u32, u32)>,
    docs: &mut Vec<u8>,
    freqs: &mut Vec<u8>,
) -> u32 {
    docs.clear();
    freqs.clear();
    let mut prev = 0u32;
    for (doc, freq) in postings {
        varbyte::encode((doc - prev) as u64, docs);
        varbyte::encode(freq as u64, freqs);
        prev = doc;
    }
    prev
}

/// Writes one run file record by record. The term count in the header is
/// patched in by [`RunWriter::finish`].
pub struct RunWriter {
    w: BufWriter<File>,
    head: Vec<u8>,
    pub num_terms: u64,
    pub bytes: u64,
}

impl RunWriter {
    pub fn create(path: &Path) -> io::Result<Self> {
        let mut w = BufWriter::with_capacity(4 << 20, File::create(path)?);
        w.write_all(MAGIC)?;
        w.write_all(&0u64.to_le_bytes())?; // placeholder, patched in finish()
        Ok(Self { w, head: Vec::with_capacity(128), num_terms: 0, bytes: 12 })
    }

    /// Append one term's record. `docs`/`freqs` come from [`encode_postings`].
    pub fn write_record(
        &mut self,
        term: &[u8],
        count: u32,
        last_doc: u32,
        docs: &[u8],
        freqs: &[u8],
    ) -> io::Result<()> {
        let h = &mut self.head;
        h.clear();
        varbyte::encode(term.len() as u64, h);
        h.extend_from_slice(term);
        varbyte::encode(count as u64, h);
        varbyte::encode(last_doc as u64, h);
        varbyte::encode(docs.len() as u64, h);
        varbyte::encode(freqs.len() as u64, h);
        self.w.write_all(h)?;
        self.w.write_all(docs)?;
        self.w.write_all(freqs)?;
        self.bytes += (h.len() + docs.len() + freqs.len()) as u64;
        self.num_terms += 1;
        Ok(())
    }

    pub fn finish(self) -> io::Result<(u64, u64)> {
        let mut f = self.w.into_inner().map_err(|e| e.into_error())?;
        f.seek(SeekFrom::Start(4))?;
        f.write_all(&self.num_terms.to_le_bytes())?;
        Ok((self.num_terms, self.bytes))
    }
}

/// One record read back from a run file.
#[derive(Default, Debug, Clone)]
pub struct RunRecord {
    pub term: Vec<u8>,
    pub count: u32,
    pub last_doc: u32,
    /// varbyte docID gaps; the first gap is relative to 0.
    pub docs: Vec<u8>,
    /// varbyte frequencies.
    pub freqs: Vec<u8>,
}

impl RunRecord {
    /// Fully decode into `(docID, freq)` pairs (debugging / tests).
    pub fn postings(&self) -> Vec<(u32, u32)> {
        let mut out = Vec::with_capacity(self.count as usize);
        let (mut dp, mut fp, mut doc) = (0usize, 0usize, 0u64);
        for _ in 0..self.count {
            doc += varbyte::decode(&self.docs, &mut dp);
            out.push((doc as u32, varbyte::decode(&self.freqs, &mut fp) as u32));
        }
        out
    }
}

/// Sequential reader over a run file. Reuses the caller's record buffers.
pub struct RunReader {
    r: BufReader<File>,
    pub num_terms: u64,
    remaining: u64,
}

impl RunReader {
    pub fn open(path: &Path) -> io::Result<Self> {
        Self::open_with_capacity(path, 1 << 20)
    }

    /// `buf_bytes` is the read-buffer size; the merger can give each run a few MB.
    pub fn open_with_capacity(path: &Path, buf_bytes: usize) -> io::Result<Self> {
        let mut r = BufReader::with_capacity(buf_bytes, File::open(path)?);
        let mut magic = [0u8; 4];
        r.read_exact(&mut magic)?;
        if &magic != MAGIC {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("{}: not a run file (bad magic)", path.display()),
            ));
        }
        let mut n = [0u8; 8];
        r.read_exact(&mut n)?;
        let num_terms = u64::from_le_bytes(n);
        Ok(Self { r, num_terms, remaining: num_terms })
    }

    /// Read the next record into `rec`. Returns `Ok(false)` after the last one.
    pub fn next_into(&mut self, rec: &mut RunRecord) -> io::Result<bool> {
        if self.remaining == 0 {
            return Ok(false);
        }
        self.remaining -= 1;
        let tl = self.vb()? as usize;
        rec.term.resize(tl, 0);
        self.r.read_exact(&mut rec.term)?;
        rec.count = self.vb()? as u32;
        rec.last_doc = self.vb()? as u32;
        let dl = self.vb()? as usize;
        let fl = self.vb()? as usize;
        rec.docs.resize(dl, 0);
        self.r.read_exact(&mut rec.docs)?;
        rec.freqs.resize(fl, 0);
        self.r.read_exact(&mut rec.freqs)?;
        Ok(true)
    }

    fn vb(&mut self) -> io::Result<u64> {
        varbyte::read(&mut self.r)?
            .ok_or_else(|| io::Error::new(io::ErrorKind::UnexpectedEof, "run file truncated"))
    }

    /// True if every byte of the file has been consumed (sanity check).
    pub fn at_eof(&mut self) -> io::Result<bool> {
        Ok(self.r.fill_buf()?.is_empty())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn write_then_read_roundtrip() {
        let dir = std::env::temp_dir().join(format!("runfile-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("run.bin");
        let lists: Vec<(&[u8], Vec<(u32, u32)>)> = vec![
            (b"apple", vec![(0, 1), (5, 3), (300, 1)]),
            (b"zebra", vec![(7, 2)]),
            (b"zzzzzzzzzlong", vec![(1, 1), (2, 1), (1_000_000, 200)]),
        ];
        let mut w = RunWriter::create(&path).unwrap();
        let (mut d, mut f) = (Vec::new(), Vec::new());
        for (t, ps) in &lists {
            let last = encode_postings(ps.iter().copied(), &mut d, &mut f);
            w.write_record(t, ps.len() as u32, last, &d, &f).unwrap();
        }
        let (terms, bytes) = w.finish().unwrap();
        assert_eq!(terms, 3);
        assert_eq!(bytes, std::fs::metadata(&path).unwrap().len());

        let mut r = RunReader::open(&path).unwrap();
        assert_eq!(r.num_terms, 3);
        let mut rec = RunRecord::default();
        for (t, ps) in &lists {
            assert!(r.next_into(&mut rec).unwrap());
            assert_eq!(rec.term, *t);
            assert_eq!(rec.postings(), *ps);
            assert_eq!(rec.last_doc, ps.last().unwrap().0);
        }
        assert!(!r.next_into(&mut rec).unwrap());
        assert!(r.at_eof().unwrap());
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
