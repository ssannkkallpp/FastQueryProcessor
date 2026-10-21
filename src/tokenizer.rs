//! Tokenization: raw passage bytes -> lowercase terms.

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
