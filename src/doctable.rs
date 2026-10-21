//! The page table (document table): docID -> passage information.
//!
//! One fixed-size 16-byte little-endian record per document, in docID order,
//! so record *d* lives at byte offset `16 * d` and can be read with one seek:

//! pid     u32   original MS MARCO passage id (u32::MAX if not numeric)
//! len     u32   document length in terms (needed by BM25)
//! offset  u64   byte offset of the passage's line in the collection file
//!               (lets the query processor fetch the text for snippets)

use std::fs::File;
use std::io::{self, BufWriter, Write};
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
