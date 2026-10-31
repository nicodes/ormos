use serde::Serialize;
use std::collections::VecDeque;
pub const HISTORY: usize = 32 << 20;
#[derive(Clone, Copy, Serialize, Debug, PartialEq, Eq)]
pub struct Size {
    pub offset: u64,
    pub cols: u16,
    pub rows: u16,
}
pub struct History {
    bytes: VecDeque<u8>,
    pub end: u64,
    sizes: VecDeque<Size>,
}
impl Default for History {
    fn default() -> Self {
        Self {
            bytes: VecDeque::new(),
            end: 0,
            sizes: VecDeque::from([Size {
                offset: 0,
                cols: 80,
                rows: 24,
            }]),
        }
    }
}
impl History {
    pub fn append(&mut self, bytes: &[u8]) {
        self.end += bytes.len() as u64;
        if bytes.len() >= HISTORY {
            self.bytes.clear();
            self.bytes.extend(&bytes[bytes.len() - HISTORY..]);
        } else {
            let excess = (self.bytes.len() + bytes.len()).saturating_sub(HISTORY);
            self.bytes.drain(..excess);
            self.bytes.extend(bytes);
        }
        while self.sizes.len() > 1 && self.sizes[1].offset <= self.start() {
            self.sizes.pop_front();
        }
    }
    pub fn start(&self) -> u64 {
        self.end - self.bytes.len() as u64
    }
    pub fn replay(&self, since: Option<u64>) -> (u64, bool) {
        match since {
            Some(since) if since >= self.start() && since <= self.end => (since, false),
            _ => (self.start(), true),
        }
    }
    pub fn chunk(&self, offset: u64, limit: usize, geometry: bool) -> Option<Vec<u8>> {
        if offset < self.start() || offset > self.end {
            return None;
        }
        let mut limit = limit.min((self.end - offset) as usize);
        if geometry && let Some(size) = self.sizes.iter().find(|size| size.offset > offset) {
            limit = limit.min((size.offset - offset) as usize);
        }
        let begin = (offset - self.start()) as usize;
        let (first, second) = self.bytes.as_slices();
        let mut bytes = Vec::with_capacity(limit);
        if begin < first.len() {
            let count = limit.min(first.len() - begin);
            bytes.extend_from_slice(&first[begin..begin + count]);
            bytes.extend_from_slice(&second[..limit - count]);
        } else {
            bytes.extend_from_slice(&second[begin - first.len()..begin - first.len() + limit]);
        }
        Some(bytes)
    }
    pub fn snapshot(&self) -> String {
        String::from_utf8_lossy(&self.bytes.iter().copied().collect::<Vec<_>>()).into_owned()
    }
    pub fn size(&self, offset: u64) -> Size {
        self.sizes
            .iter()
            .rev()
            .find(|size| size.offset <= offset)
            .copied()
            .unwrap_or(Size {
                offset: 0,
                cols: 80,
                rows: 24,
            })
    }
    pub fn resize(&mut self, cols: u16, rows: u16) {
        let previous = self.size(self.end);
        if previous.cols == cols && previous.rows == rows {
            return;
        }
        let size = Size {
            offset: self.end,
            cols,
            rows,
        };
        if self.sizes.back().is_some_and(|s| s.offset == self.end) {
            *self.sizes.back_mut().unwrap() = size;
        } else {
            self.sizes.push_back(size);
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn absolute_replay_offsets_and_geometry_survive_growth_and_eviction() {
        let mut h = History::default();
        h.append(b"first");
        h.resize(120, 40);
        h.append(b"second");
        assert_eq!(h.chunk(0, 100, true).unwrap(), b"first");
        assert_eq!(h.chunk(5, 100, true).unwrap(), b"second");
        assert_eq!(h.size(5).cols, 120);
        h.append(&vec![b'x'; HISTORY]);
        assert_eq!(h.start(), 11);
        assert_eq!(h.replay(Some(1)), (11, true));
        assert!(h.chunk(1, 100, false).is_none());
        assert_eq!(h.size(11).cols, 120);
        assert_eq!(h.replay(Some(h.end)), (h.end, false));
    }
}
