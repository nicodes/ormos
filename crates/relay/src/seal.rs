use crate::{FRAME_HEADER_SIZE, MAX_FRAME_SIZE, TermFrame, decode_frame};
use chacha20poly1305::{
    ChaCha20Poly1305,
    aead::{Aead, KeyInit},
};
use hkdf::Hkdf;
use sha2::{Digest, Sha256};
use std::io::{self, Read, Write};
use x25519_dalek::{PublicKey, StaticSecret};
use zeroize::Zeroizing;
pub const SEAL_KEY_SIZE: usize = 32;
pub const SEAL_SALT_SIZE: usize = 32;
pub const SEAL_INFO: &str = "ormos terminal seal v2";
pub const MAX_SEALED_RECORD: usize = MAX_FRAME_SIZE + FRAME_HEADER_SIZE + 16;
pub const MAX_REUSED_RECORD: usize = 64 << 10;
fn failed() -> io::Error {
    io::Error::new(
        io::ErrorKind::InvalidData,
        "sealed record failed authentication",
    )
}
pub struct AgentKey(StaticSecret);
impl AgentKey {
    pub fn generate() -> io::Result<Self> {
        let mut bytes = Zeroizing::new([0_u8; 32]);
        getrandom::fill(bytes.as_mut())
            .map_err(|_| io::Error::other("generating agent key failed"))?;
        Ok(Self(StaticSecret::from(*bytes)))
    }
    pub fn from_bytes(bytes: [u8; 32]) -> Self {
        Self(StaticSecret::from(bytes))
    }
    pub fn to_bytes(&self) -> [u8; 32] {
        self.0.to_bytes()
    }
    pub fn public_key(&self) -> [u8; 32] {
        PublicKey::from(&self.0).to_bytes()
    }
}
pub struct SessionKeys {
    pub client_to_agent: Zeroizing<[u8; 32]>,
    pub agent_to_client: Zeroizing<[u8; 32]>,
}
pub fn derive_session_keys(
    private: &AgentKey,
    peer: &[u8],
    salt: &[u8],
    session: &str,
) -> io::Result<SessionKeys> {
    let peer: [u8; 32] = peer.try_into().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "peer public key must be 32 bytes",
        )
    })?;
    if salt.len() != 32 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "server salt must be 32 bytes",
        ));
    }
    let shared = private.0.diffie_hellman(&PublicKey::from(peer));
    if !shared.was_contributory() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "key agreement failed",
        ));
    }
    let own = private.public_key();
    let (lo, hi) = if own < peer {
        (&own, &peer)
    } else {
        (&peer, &own)
    };
    let mut info = Vec::new();
    info.extend_from_slice(SEAL_INFO.as_bytes());
    info.push(0);
    info.extend_from_slice(lo);
    info.extend_from_slice(hi);
    info.extend_from_slice(session.as_bytes());
    let mut output = Zeroizing::new([0_u8; 64]);
    Hkdf::<Sha256>::new(Some(salt), shared.as_bytes())
        .expand(&info, output.as_mut())
        .map_err(|_| io::Error::other("key derivation failed"))?;
    let mut client = Zeroizing::new([0; 32]);
    client.copy_from_slice(&output[..32]);
    let mut agent = Zeroizing::new([0; 32]);
    agent.copy_from_slice(&output[32..]);
    Ok(SessionKeys {
        client_to_agent: client,
        agent_to_client: agent,
    })
}
pub fn fingerprint(public: &[u8]) -> String {
    let digest = Sha256::digest(public);
    let text = data_encoding::BASE32_NOPAD.encode(&digest[..10]);
    text.as_bytes()
        .chunks(4)
        .map(|chunk| std::str::from_utf8(chunk).expect("base32 alphabet"))
        .collect::<Vec<_>>()
        .join("-")
}
pub struct Sealer {
    cipher: ChaCha20Poly1305,
    counter: Option<u64>,
}
impl Sealer {
    pub fn new(key: &[u8]) -> io::Result<Self> {
        Ok(Self {
            cipher: ChaCha20Poly1305::new_from_slice(key).map_err(|_| failed())?,
            counter: Some(0),
        })
    }
    fn nonce(&mut self) -> io::Result<[u8; 12]> {
        let counter = self.counter.ok_or_else(failed)?;
        self.counter = counter.checked_add(1);
        let mut nonce = [0; 12];
        nonce[4..].copy_from_slice(&counter.to_be_bytes());
        Ok(nonce)
    }
    pub fn seal(&mut self, plaintext: &[u8]) -> io::Result<Vec<u8>> {
        if plaintext.len() > MAX_FRAME_SIZE + FRAME_HEADER_SIZE {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "sealed plaintext exceeds its bound",
            ));
        }
        let nonce = self.nonce()?;
        self.cipher
            .encrypt(&nonce.into(), plaintext)
            .map_err(|_| failed())
    }
    pub fn open(&mut self, record: &[u8]) -> io::Result<Vec<u8>> {
        let nonce = self.nonce()?;
        self.cipher
            .decrypt(&nonce.into(), record)
            .map_err(|_| failed())
    }
}
pub fn write_record(writer: &mut impl Write, record: &[u8]) -> io::Result<()> {
    let length: u32 = record.len().try_into().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "record length exceeds wire format",
        )
    })?;
    let mut bytes = Vec::with_capacity(4 + record.len());
    bytes.extend_from_slice(&length.to_be_bytes());
    bytes.extend_from_slice(record);
    writer.write_all(&bytes)
}
fn record_length(reader: &mut impl Read) -> io::Result<usize> {
    let mut header = [0; 4];
    reader.read_exact(&mut header)?;
    let size = u32::from_be_bytes(header) as usize;
    if size > MAX_SEALED_RECORD {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "sealed record length exceeds its bound",
        ));
    }
    Ok(size)
}
pub fn read_record(reader: &mut impl Read) -> io::Result<Vec<u8>> {
    let size = record_length(reader)?;
    let mut record = vec![0; size];
    reader.read_exact(&mut record)?;
    Ok(record)
}
pub fn read_record_into<'a>(
    reader: &mut impl Read,
    buffer: &'a mut Vec<u8>,
) -> io::Result<&'a [u8]> {
    let size = record_length(reader)?;
    if size > buffer.capacity() {
        buffer.reserve_exact(size - buffer.len());
    }
    buffer.resize(size, 0);
    reader.read_exact(buffer)?;
    Ok(buffer)
}
pub fn write_client_hello(writer: &mut impl Write, public: &[u8]) -> io::Result<()> {
    if public.len() != 32 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "client hello must be 32 bytes",
        ));
    }
    write_record(writer, public)
}
pub fn read_client_hello(reader: &mut impl Read) -> io::Result<[u8; 32]> {
    read_record(reader)?
        .try_into()
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "client hello must be 32 bytes"))
}
pub fn generate_server_salt() -> io::Result<[u8; 32]> {
    let mut salt = [0; 32];
    getrandom::fill(&mut salt).map_err(|_| io::Error::other("generating server salt failed"))?;
    Ok(salt)
}
pub fn write_server_hello(writer: &mut impl Write, salt: &[u8]) -> io::Result<()> {
    if salt.len() != 32 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "server hello must be 32 bytes",
        ));
    }
    write_record(writer, salt)
}
pub fn read_server_hello(reader: &mut impl Read) -> io::Result<[u8; 32]> {
    read_record(reader)?
        .try_into()
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "server hello must be 32 bytes"))
}
pub struct SealedStream<R, W> {
    reader: R,
    writer: W,
    send: Sealer,
    recv: Sealer,
    buffer: Vec<u8>,
}
impl<R: Read, W: Write> SealedStream<R, W> {
    pub fn new(reader: R, writer: W, send: &[u8], recv: &[u8]) -> io::Result<Self> {
        Ok(Self {
            reader,
            writer,
            send: Sealer::new(send)?,
            recv: Sealer::new(recv)?,
            buffer: Vec::new(),
        })
    }
    pub fn write_frame(&mut self, frame: &[u8]) -> io::Result<()> {
        write_record(&mut self.writer, &self.send.seal(frame)?)
    }
    pub fn read_frame(&mut self) -> io::Result<TermFrame> {
        let size = record_length(&mut self.reader)?;
        let plaintext = if size > MAX_REUSED_RECORD {
            let mut temporary = vec![0; size];
            self.reader.read_exact(&mut temporary)?;
            self.recv.open(&temporary)?
        } else {
            if size > self.buffer.capacity() {
                self.buffer.reserve_exact(size - self.buffer.len());
            }
            self.buffer.resize(size, 0);
            self.reader.read_exact(&mut self.buffer)?;
            self.recv.open(&self.buffer)?
        };
        decode_frame(&plaintext)
    }
    pub fn into_parts(self) -> (R, W) {
        (self.reader, self.writer)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;
    #[test]
    fn nonce_exhaustion_refuses_reuse_and_large_pastes_do_not_pin_a_large_buffer() {
        let key = [7; 32];
        let mut exhausted = Sealer::new(&key).unwrap();
        exhausted.counter = Some(u64::MAX);
        assert!(exhausted.seal(b"last nonce").is_ok());
        assert!(exhausted.seal(b"must never reuse nonce zero").is_err());
        let mut sender = Sealer::new(&key).unwrap();
        let mut wire = Vec::new();
        for size in [3, MAX_FRAME_SIZE, 9] {
            write_record(
                &mut wire,
                &sender.seal(&crate::encode_data(&vec![1; size])).unwrap(),
            )
            .unwrap();
        }
        let mut stream = SealedStream::new(Cursor::new(wire), io::sink(), &key, &key).unwrap();
        for _ in 0..3 {
            stream.read_frame().unwrap();
            assert!(stream.buffer.capacity() <= MAX_REUSED_RECORD);
        }
    }
}
