//! Variable-byte (varbyte / LEB128-style) integer coding.
//!
//! Each byte carries 7 payload bits, least-significant group first. The high
//! bit is a continuation flag: 1 = more bytes follow, 0 = this is the last
//! byte. Small numbers (docID gaps, term frequencies) therefore take a single
//! byte, which is what makes this a good fit for inverted lists.
//!
//! Examples:  5 -> [0x05]      300 -> [0xAC, 0x02]

use std::io::{self, BufRead};

/// Append the varbyte encoding of `v` to `out`.
#[inline]
pub fn encode(mut v: u64, out: &mut Vec<u8>) {
    while v >= 0x80 {
        out.push((v as u8) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

/// Decode one integer from `buf` starting at `*pos`, advancing `*pos`.
/// Panics if the buffer ends in the middle of a number (corrupt input).
#[inline]
pub fn decode(buf: &[u8], pos: &mut usize) -> u64 {
    let mut v = 0u64;
    let mut shift = 0u32;
    loop {
        let b = buf[*pos];
        *pos += 1;
        v |= ((b & 0x7f) as u64) << shift;
        if b < 0x80 {
            return v;
        }
        shift += 7;
    }
}

/// Decode one integer directly from a buffered reader.
/// Returns `Ok(None)` on a clean end-of-file before the first byte.
pub fn read<R: BufRead>(r: &mut R) -> io::Result<Option<u64>> {
    let mut v = 0u64;
    let mut shift = 0u32;
    let mut first = true;
    loop {
        let b = {
            let buf = r.fill_buf()?;
            if buf.is_empty() {
                if first {
                    return Ok(None);
                }
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "truncated varbyte integer",
                ));
            }
            buf[0]
        };
        r.consume(1);
        first = false;
        if shift >= 64 {
            return Err(io::Error::new(io::ErrorKind::InvalidData, "varbyte overflow"));
        }
        v |= ((b & 0x7f) as u64) << shift;
        if b < 0x80 {
            return Ok(Some(v));
        }
        shift += 7;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let values = [0u64, 1, 127, 128, 300, 16_383, 16_384, 8_841_822, u32::MAX as u64, u64::MAX];
        let mut buf = Vec::new();
        for &v in &values {
            encode(v, &mut buf);
        }
        let mut pos = 0;
        for &v in &values {
            assert_eq!(decode(&buf, &mut pos), v);
        }
        assert_eq!(pos, buf.len());

        let mut cur = io::Cursor::new(&buf);
        for &v in &values {
            assert_eq!(read(&mut cur).unwrap(), Some(v));
        }
        assert_eq!(read(&mut cur).unwrap(), None);
    }

    #[test]
    fn known_encodings() {
        let mut b = Vec::new();
        encode(5, &mut b);
        assert_eq!(b, [0x05]);
        b.clear();
        encode(300, &mut b);
        assert_eq!(b, [0xAC, 0x02]);
    }
}
