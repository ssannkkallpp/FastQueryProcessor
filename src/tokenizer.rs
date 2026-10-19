//! Tokenization: raw passage bytes -> lowercase terms.
//!
//! * ASCII letters and digits form terms; letters are lowercased.
//! * Every other ASCII byte (space, punctuation, symbols, control chars) is a
//!   separator, so "don't" -> ["don", "t"] and "e-mail" -> ["e", "mail"].
//! * A token containing any non-ASCII byte (>= 0x80) is dropped whole. This
//!   "just ignores" text in other scripts, as the assignment allows, without
//!   leaving junk fragments behind ("café" is dropped, not indexed as "caf"),
//!   and works on arbitrary bytes, so malformed UTF-8 can never cause a failure.
//! * Tokens longer than [`MAX_TERM_LEN`] bytes are dropped.
//!
//! No stemming and no stopword removal: every remaining term is indexed.

/// Longest term (in bytes) that is indexed; longer tokens are dropped.
pub const MAX_TERM_LEN: usize = 64;

const SEP: u8 = 0; // separator byte
const BAD: u8 = 1; // non-ASCII byte: poisons the current token

/// Byte class table: SEP, BAD, or the lowercased term character.
const CLASS: [u8; 256] = {
    let mut t = [SEP; 256];
    let mut c = 0;
    while c < 256 {
        let b = c as u8;
        t[c] = match b {
            b'a'..=b'z' | b'0'..=b'9' => b,
            b'A'..=b'Z' => b + (b'a' - b'A'),
            0x80..=0xff => BAD,
            _ => SEP,
        };
        c += 1;
    }
    t
};

/// Reusable tokenizer holding the scratch buffer for the term being built.
pub struct Tokenizer {
    term: [u8; MAX_TERM_LEN],
}

impl Default for Tokenizer {
    fn default() -> Self {
        Self::new()
    }
}

impl Tokenizer {
    pub fn new() -> Self {
        Self { term: [0; MAX_TERM_LEN] }
    }

    /// Call `emit` once for every term in `text`, in order of occurrence.
    /// The slice passed to `emit` is only valid during the call.
    #[inline]
    pub fn tokenize<F: FnMut(&[u8])>(&mut self, text: &[u8], mut emit: F) {
        let mut len = 0usize; // bytes in the current token
        let mut bad = false; // current token contains non-ASCII or is too long
        for &c in text {
            match CLASS[c as usize] {
                SEP => {
                    if len > 0 && !bad {
                        emit(&self.term[..len]);
                    }
                    len = 0;
                    bad = false;
                }
                BAD => bad = true,
                lc => {
                    if len < MAX_TERM_LEN {
                        self.term[len] = lc;
                        len += 1;
                    } else {
                        bad = true;
                    }
                }
            }
        }
        if len > 0 && !bad {
            emit(&self.term[..len]);
        }
    }
}

/// Convenience wrapper returning owned strings (tests, and short user queries).
pub fn tokenize_to_vec(text: &[u8]) -> Vec<String> {
    let mut out = Vec::new();
    Tokenizer::new().tokenize(text, |t| out.push(String::from_utf8(t.to_vec()).unwrap()));
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ascii() {
        assert_eq!(
            tokenize_to_vec(b"The U.S. economy, in 2019, grew 2.3% -- don't panic!"),
            ["the", "u", "s", "economy", "in", "2019", "grew", "2", "3", "don", "t", "panic"]
        );
    }

    #[test]
    fn non_ascii_tokens_dropped_whole() {
        assert_eq!(tokenize_to_vec("Café au lait, 東京 tower".as_bytes()), ["au", "lait", "tower"]);
        assert_eq!(tokenize_to_vec(b"abc\xff\xfedef ok \xc3 end"), ["ok", "end"]);
    }

    #[test]
    fn long_tokens_dropped() {
        let long = "x".repeat(MAX_TERM_LEN + 1);
        assert_eq!(tokenize_to_vec(format!("a {long} b").as_bytes()), ["a", "b"]);
        let ok = "y".repeat(MAX_TERM_LEN);
        assert_eq!(tokenize_to_vec(ok.as_bytes()), [ok.clone()]);
    }

    #[test]
    fn empty_and_separators_only() {
        assert!(tokenize_to_vec(b"").is_empty());
        assert!(tokenize_to_vec(b" ,.;\t-- ").is_empty());
    }
}
