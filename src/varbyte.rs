//! Variable-byte (varbyte / LEB128-style) integer coding.
//!
//! Each byte carries 7 payload bits, least-significant group first. The high
//! bit is a continuation flag: 1 = more bytes follow, 0 = this is the last
//! byte. Small numbers (docID gaps, term frequencies) therefore take a single
//! byte, which is what makes this a good fit for inverted lists.
//!
//! Examples:  5 -> [0x05]      300 -> [0xAC, 0x02]

/// Append the varbyte encoding of `v` to `out`.
#[inline]
pub fn encode(mut v: u64, out: &mut Vec<u8>) {
    while v >= 0x80 {
        out.push((v as u8) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}
