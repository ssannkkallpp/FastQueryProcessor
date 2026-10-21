//! Rust modules used by the `parse` executable.
//! The Go merger and query processor exchange data with Rust through files;
//! they do not link against this library.
//!
//! * [`tokenizer`]: turns raw passage bytes into lowercase terms. The Go query
//!   processor currently uses lowercase whitespace splitting instead.
//! * [`varbyte`]: variable-byte integer coding used for all postings.
//! * [`runfile`]: the intermediate "run" format written by `parse` and
//!   consumed by `merge`.
//! * [`doctable`]: the page table (docID -> passage id, length, file offset).

pub mod doctable;
pub mod runfile;
pub mod tokenizer;
pub mod varbyte;
