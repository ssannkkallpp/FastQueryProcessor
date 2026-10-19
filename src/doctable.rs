//! The page table (document table): docID -> passage information.
//!
//! One fixed-size 16-byte little-endian record per document, in docID order,
//! so record *d* lives at byte offset `16 * d` and can be read with one seek:
//!
//! ```text
//! pid     u32   original MS MARCO passage id (u32::MAX if not numeric)
//! len     u32   document length in terms (needed by BM25)
//! offset  u64   byte offset of the passage's line in the collection file
//!               (lets the query processor fetch the text for snippets)
//! ```
//!
//! For MS MARCO the docID normally equals the pid because docIDs are assigned
//! in file order, but the table makes no such assumption.

use std::fs::File;
use std::io::{self, BufReader, BufWriter, Read, Write};
use std::path::Path;

pub const RECORD_BYTES: usize = 16;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct DocRecord {
    pub pid: u32,
    pub len: u32,
    pub offset: u64,
}

impl DocRecord {
    pub fn to_bytes(&self) -> [u8; RECORD_BYTES] {
        let mut b = [0u8; RECORD_BYTES];
        b[0..4].copy_from_slice(&self.pid.to_le_bytes());
        b[4..8].copy_from_slice(&self.len.to_le_bytes());
        b[8..16].copy_from_slice(&self.offset.to_le_bytes());
        b
    }

    pub fn from_bytes(b: &[u8]) -> Self {
        Self {
            pid: u32::from_le_bytes(b[0..4].try_into().unwrap()),
            len: u32::from_le_bytes(b[4..8].try_into().unwrap()),
            offset: u64::from_le_bytes(b[8..16].try_into().unwrap()),
        }
    }
}

/// Appends records in docID order.
pub struct DocTableWriter {
    w: BufWriter<File>,
    pub count: u64,
}

impl DocTableWriter {
    pub fn create(path: &Path) -> io::Result<Self> {
        Ok(Self { w: BufWriter::with_capacity(1 << 20, File::create(path)?), count: 0 })
    }

    pub fn push(&mut self, rec: DocRecord) -> io::Result<()> {
        self.count += 1;
        self.w.write_all(&rec.to_bytes())
    }

    pub fn finish(mut self) -> io::Result<()> {
        self.w.flush()
    }
}

/// Load the whole table (~140 MB for MS MARCO; the query processor may prefer
/// to keep only the `len` column in memory and seek for the rest).
pub fn read_all(path: &Path) -> io::Result<Vec<DocRecord>> {
    let mut r = BufReader::with_capacity(4 << 20, File::open(path)?);
    let mut out = Vec::new();
    let mut b = [0u8; RECORD_BYTES];
    loop {
        match r.read_exact(&mut b) {
            Ok(()) => out.push(DocRecord::from_bytes(&b)),
            Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => return Ok(out),
            Err(e) => return Err(e),
        }
    }
}
