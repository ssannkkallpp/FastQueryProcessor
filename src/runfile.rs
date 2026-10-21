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

use crate::varbyte;
use std::fs::File;
use std::io::{self, BufWriter, Seek, SeekFrom, Write};
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
