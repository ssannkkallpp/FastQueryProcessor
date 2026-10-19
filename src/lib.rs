//! The three executables (parse -> merge -> query) all link against this
//! library so they agree on tokenization, compression and file formats:
//!
//! * [`tokenizer`]:  turns raw passage bytes into lowercase terms. The query
//!   processor must use exactly the same function on user queries.
//! * [`varbyte`]: variable-byte integer coding used for all postings.
//! * [`runfile`]: the intermediate "run" format written by `parse` and
//!   consumed by `merge`.
//! * [`doctable`]: the page table (docID -> passage id, length, file offset).

pub mod doctable;
pub mod runfile;
pub mod tokenizer;
pub mod varbyte;
