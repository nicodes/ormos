use crate::{ActionAck, Resize, StreamHeader};
use serde::{
    Deserialize,
    de::{MapAccess, Visitor},
};
use serde_json::Value;
use std::{
    collections::BTreeSet,
    fmt,
    io::{self, BufRead, BufReader, Read, Write},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
pub const MAX_FRAME_SIZE: usize = 1 << 20;
pub const MAX_HEADER_SIZE: usize = 64 << 10;
pub const FRAME_HEADER_SIZE: usize = 5;
pub const MIN_TERMINAL_DIM: i64 = 1;
pub const MAX_TERMINAL_DIM: i64 = 1000;
pub const MIN_PORT: i64 = 1;
pub const MAX_PORT: i64 = 65535;
pub const MAX_TUNNEL_STREAMS: usize = 128;
pub const MAX_STREAM_WINDOW: usize = 4 << 20;
pub const MAX_TUNNEL_WINDOW_BYTES: usize = MAX_STREAM_WINDOW * MAX_TUNNEL_STREAMS;
fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}
pub fn parse_stream_fence_version_header(values: &[String]) -> io::Result<&str> {
    match values {
        [] => Ok(""),
        [version] if matches!(version.as_str(), "1" | "2" | "3" | "4") => Ok(version),
        [_] => Err(invalid("unsupported stream-fence version")),
        _ => Err(invalid(
            "stream-fence version header must occur exactly once",
        )),
    }
}
pub fn valid_terminal_size(cols: i64, rows: i64) -> bool {
    (MIN_TERMINAL_DIM..=MAX_TERMINAL_DIM).contains(&cols)
        && (MIN_TERMINAL_DIM..=MAX_TERMINAL_DIM).contains(&rows)
}
pub fn valid_port(port: i64) -> bool {
    (MIN_PORT..=MAX_PORT).contains(&port)
}
pub fn validate_v4_stream_header(
    header: &StreamHeader,
    authenticated_system: &str,
) -> io::Result<()> {
    if header.protocol_version != "4" {
        return Err(invalid("stream protocol version is not v4"));
    }
    if authenticated_system.is_empty()
        || header.system_id.is_empty()
        || header.system_id != authenticated_system
    {
        return Err(invalid("v4 stream system identity mismatch"));
    }
    if !header.session_id.is_empty() {
        return Err(invalid(
            "v4 stream must not use project-composite session identity",
        ));
    }
    match header.kind.as_str() {
        "terminal" => {
            if header.terminal_record_id.is_empty() {
                return Err(invalid("v4 terminal is missing its record identity"));
            }
            if header.terminal_generation <= 0 {
                return Err(invalid("v4 terminal generation must be positive"));
            }
            if header.cwd.is_empty() {
                return Err(invalid("v4 terminal is missing its working directory"));
            }
        }
        "proxy" if !valid_port(header.port) => return Err(invalid("v4 proxy port is invalid")),
        "proxy" | "shutdown" => {}
        _ => {
            return Err(invalid(
                "v4 direct-system identity is not valid for stream kind",
            ));
        }
    }
    Ok(())
}
pub fn terminal_session_binding(header: &StreamHeader) -> io::Result<String> {
    if header.protocol_version != "4" {
        return Ok(header.session_id.clone());
    }
    if header.system_id.is_empty()
        || header.terminal_record_id.is_empty()
        || header.terminal_generation <= 0
        || header.cwd.is_empty()
        || header.action_fence.is_empty()
        || header.not_after_milli <= 0
    {
        return Err(invalid("incomplete v4 terminal binding"));
    }
    Ok(format!(
        "v4\0{}\0{}\0{}\0{}\0{}\0{}",
        header.system_id,
        header.terminal_record_id,
        header.terminal_generation,
        header.cwd,
        header.action_fence,
        header.not_after_milli
    ))
}
pub fn terminal_resource_identity(header: &StreamHeader) -> io::Result<String> {
    if header.protocol_version != "4" {
        return Ok(header.session_id.clone());
    }
    if header.system_id.is_empty()
        || header.terminal_record_id.is_empty()
        || header.terminal_generation <= 0
    {
        return Err(invalid("incomplete v4 terminal resource identity"));
    }
    Ok(format!(
        "v4\0{}\0{}\0{}",
        header.system_id, header.terminal_record_id, header.terminal_generation
    ))
}
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FenceError {
    Expired,
    Invalid(&'static str),
}
impl fmt::Display for FenceError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Expired => f.write_str("stream action fence expired"),
            Self::Invalid(message) => f.write_str(message),
        }
    }
}
impl std::error::Error for FenceError {}
pub fn stream_kind_requires_fence(kind: &str) -> bool {
    matches!(kind, "terminal" | "proxy" | "shutdown")
}
pub struct FenceAcceptance {
    pub deadline: Option<Instant>,
    pub result: Result<(), FenceError>,
}
pub fn accept_stream_fence(
    header: &StreamHeader,
    wall: SystemTime,
    accepted_at: Instant,
) -> FenceAcceptance {
    if !stream_kind_requires_fence(&header.kind) {
        return FenceAcceptance {
            deadline: None,
            result: Ok(()),
        };
    }
    let expiry = if header.not_after_milli >= 0 {
        UNIX_EPOCH.checked_add(Duration::from_millis(header.not_after_milli as u64))
    } else {
        UNIX_EPOCH.checked_sub(Duration::from_millis(header.not_after_milli.unsigned_abs()))
    };
    let remaining = expiry
        .and_then(|time| time.duration_since(wall).ok())
        .unwrap_or_default();
    let maximum = Duration::from_secs(60);
    let deadline = Some(accepted_at + remaining.min(maximum));
    let result = if !(32..=64).contains(&header.action_fence.len()) {
        Err(FenceError::Invalid(
            "stream action fence has invalid length",
        ))
    } else if !header
        .action_fence
        .bytes()
        .all(|b| b.is_ascii_alphanumeric())
    {
        Err(FenceError::Invalid(
            "stream action fence has invalid characters",
        ))
    } else if remaining.is_zero() {
        Err(FenceError::Expired)
    } else if remaining > maximum {
        Err(FenceError::Invalid(
            "stream action fence is too far in the future",
        ))
    } else {
        Ok(())
    };
    FenceAcceptance { deadline, result }
}
pub fn validate_stream_fence_deadline(deadline: Instant, now: Instant) -> Result<(), FenceError> {
    if now < deadline {
        Ok(())
    } else {
        Err(FenceError::Expired)
    }
}
pub fn new_action_ack(header: &StreamHeader, status: impl Into<String>) -> ActionAck {
    ActionAck {
        action_fence: header.action_fence.clone(),
        not_after_milli: header.not_after_milli,
        status: status.into(),
    }
}
pub fn validate_action_ack(header: &StreamHeader, ack: &ActionAck) -> io::Result<()> {
    if header.kind != "shutdown" {
        return Err(invalid("action acknowledgment is only valid for shutdown"));
    }
    if header.action_fence != ack.action_fence || header.not_after_milli != ack.not_after_milli {
        return Err(invalid(
            "action acknowledgment does not match its shutdown fence",
        ));
    }
    if !matches!(ack.status.as_str(), "success" | "refused" | "expired") {
        return Err(invalid("unknown action acknowledgment status"));
    }
    Ok(())
}
pub fn write_action_ack(writer: &mut impl Write, ack: &ActionAck) -> io::Result<()> {
    let mut data = serde_json::to_vec(ack)?;
    data.push(b'\n');
    writer.write_all(&data)
}
pub fn write_header(writer: &mut impl Write, header: &StreamHeader) -> io::Result<()> {
    let mut data = serde_json::to_vec(header)?;
    data.push(b'\n');
    writer.write_all(&data)
}
fn line(reader: &mut impl BufRead) -> io::Result<Vec<u8>> {
    let mut data = Vec::new();
    std::io::Read::take(reader, (MAX_HEADER_SIZE + 2) as u64).read_until(b'\n', &mut data)?;
    if data.last() != Some(&b'\n') {
        return Err(if data.len() > MAX_HEADER_SIZE {
            invalid("stream protocol message exceeds its bound")
        } else {
            io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "stream protocol message is missing its newline",
            )
        });
    }
    data.pop();
    if data.len() > MAX_HEADER_SIZE {
        return Err(invalid("stream protocol message exceeds its bound"));
    }
    Ok(data)
}
pub fn read_action_ack(reader: impl Read) -> io::Result<ActionAck> {
    serde_json::from_slice(&line(&mut BufReader::new(reader))?)
        .map_err(|_| invalid("decode action acknowledgment"))
}
struct Unique(Vec<(String, Value)>);
impl<'de> Deserialize<'de> for Unique {
    fn deserialize<D: serde::Deserializer<'de>>(decoder: D) -> Result<Self, D::Error> {
        struct Object;
        impl<'de> Visitor<'de> for Object {
            type Value = Unique;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a stream header object")
            }
            fn visit_map<M: MapAccess<'de>>(self, mut map: M) -> Result<Self::Value, M::Error> {
                let mut seen = BTreeSet::new();
                let mut values = Vec::new();
                while let Some((name, value)) = map.next_entry::<String, Value>()? {
                    if !seen.insert(name.clone()) {
                        return Err(serde::de::Error::custom("duplicate stream header field"));
                    }
                    values.push((name, value));
                }
                Ok(Unique(values))
            }
        }
        decoder.deserialize_map(Object)
    }
}
pub fn read_header<R: Read>(reader: R) -> io::Result<(StreamHeader, BufReader<R>)> {
    let mut reader = BufReader::new(reader);
    let fields: Unique = serde_json::from_slice(&line(&mut reader)?)
        .map_err(|_| invalid("decode stream header: malformed or duplicate fields"))?;
    let mut object = serde_json::Map::new();
    for (key, value) in fields.0 {
        object.insert(key.to_ascii_lowercase(), value);
    }
    let version = match object.get("protocol_version") {
        None | Some(Value::Null) => "",
        Some(Value::String(version)) => version.as_str(),
        _ => return Err(invalid("decode stream header")),
    };
    if !matches!(version, "" | "3" | "4") {
        return Err(invalid("unsupported stream protocol version"));
    }
    if version == "4"
        && object.keys().any(|name| {
            ![
                "kind",
                "protocol_version",
                "system_id",
                "port",
                "cols",
                "rows",
                "cwd",
                "session_id",
                "terminal_record_id",
                "terminal_generation",
                "action_fence",
                "not_after_milli",
            ]
            .contains(&name.as_str())
        })
    {
        return Err(invalid("decode v4 stream header: unknown field"));
    }
    let header = serde_json::from_value(Value::Object(object))
        .map_err(|_| invalid("decode stream header"))?;
    Ok((header, reader))
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum TermFrame {
    Data(Vec<u8>),
    Resize(Resize),
    Activity(bool),
}
fn encode_frame(tag: u8, payload: &[u8]) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(FRAME_HEADER_SIZE + payload.len());
    bytes.push(tag);
    bytes.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    bytes.extend_from_slice(payload);
    bytes
}
pub fn encode_data(payload: &[u8]) -> Vec<u8> {
    encode_frame(0, payload)
}
pub fn encode_resize(cols: i64, rows: i64) -> Vec<u8> {
    encode_frame(
        1,
        &serde_json::to_vec(&Resize { cols, rows }).expect("resize serialization"),
    )
}
pub fn encode_activity(active: bool) -> Vec<u8> {
    encode_frame(
        2,
        if active {
            b"{\"active\":true}"
        } else {
            b"{\"active\":false}"
        },
    )
}
pub fn decode_frame(bytes: &[u8]) -> io::Result<TermFrame> {
    if bytes.len() < FRAME_HEADER_SIZE {
        return Err(invalid("terminal frame shorter than its header"));
    }
    let length = u32::from_be_bytes(bytes[1..5].try_into().expect("frame length")) as usize;
    if length > MAX_FRAME_SIZE {
        return Err(invalid("terminal frame length exceeds its bound"));
    }
    let payload = bytes
        .get(5..5 + length)
        .ok_or_else(|| invalid("terminal frame length overruns bytes present"))?;
    match bytes[0] {
        0 => Ok(TermFrame::Data(payload.to_vec())),
        1 => Ok(TermFrame::Resize(
            serde_json::from_slice(payload).map_err(|_| invalid("decode resize frame"))?,
        )),
        2 => {
            #[derive(Default, Deserialize)]
            #[serde(default)]
            struct Activity {
                active: Option<bool>,
            }
            let activity: Activity =
                serde_json::from_slice(payload).map_err(|_| invalid("decode activity frame"))?;
            Ok(TermFrame::Activity(activity.active.unwrap_or(false)))
        }
        _ => Err(invalid("unknown terminal frame tag")),
    }
}
