use crate::{
    error::{Result, fail, invalid},
    terminal::Terminal,
    workspace::{Viewer, Workspace},
};
use axum::{
    extract::{FromRequestParts, Request},
    response::{IntoResponse, Response},
};
use bytes::Bytes;
use futures_util::{SinkExt, StreamExt};
use serde::Deserialize;
use serde_json::json;
use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{sync::Notify, time::Instant};
use yawc::{Frame, OpCode, close::CloseCode};
const WINDOW: usize = 256 << 10;
const INPUT: usize = 32 << 10;
#[derive(Default, Debug)]
pub struct Options {
    since: Option<u64>,
    flow: bool,
    input: bool,
    geometry: bool,
}
impl Options {
    pub fn parse(query: &str) -> Result<Self> {
        let mut result = Self::default();
        let mut seen = std::collections::BTreeSet::new();
        let url = reqwest::Url::parse(&format!("http://localhost/?{query}"))
            .map_err(|_| invalid("invalid terminal query"))?;
        for (key, value) in url.query_pairs() {
            if !matches!(key.as_ref(), "since" | "flow" | "input" | "geometry") {
                continue;
            }
            if !seen.insert(key.to_string()) {
                return Err(invalid("duplicate terminal query"));
            }
            if key == "since" {
                if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
                    return Err(invalid("invalid terminal offset"));
                }
                let offset = value
                    .parse::<u64>()
                    .map_err(|_| invalid("invalid terminal offset"))?;
                if offset > 9_007_199_254_740_991 {
                    return Err(invalid("invalid terminal offset"));
                }
                result.since = Some(offset);
            } else {
                if value != "1" {
                    return Err(invalid("invalid terminal option"));
                }
                match key.as_ref() {
                    "flow" => result.flow = true,
                    "input" => result.input = true,
                    "geometry" => result.geometry = true,
                    _ => {}
                }
            }
        }
        if result.geometry && !result.flow {
            return Err(invalid("terminal geometry requires flow control"));
        }
        Ok(result)
    }
}
struct StreamState {
    pending: usize,
    queue: VecDeque<u8>,
    input_total: usize,
    input_ack: usize,
    closed: Option<(CloseCode, &'static str)>,
}
struct Shared {
    state: Mutex<StreamState>,
    wake: Notify,
    input: Notify,
}
impl Shared {
    fn close(&self, code: CloseCode, reason: &'static str) {
        self.state
            .lock()
            .unwrap()
            .closed
            .get_or_insert((code, reason));
        self.wake.notify_waiters();
        self.input.notify_one();
    }
}
#[derive(Default, Deserialize)]
#[serde(default)]
struct Message {
    r#type: String,
    data: String,
    cols: u16,
    rows: u16,
    bytes: usize,
}

pub async fn upgrade(workspace: Arc<Workspace>, id: &str, request: Request) -> Result<Response> {
    let policy = workspace.config.policy()?;
    if policy.terminals_disabled {
        return Err(fail(403, "local policy disables terminals"));
    }
    let terminal = workspace.terminal(id)?;
    if !terminal.info().alive {
        return Err(fail(410, "terminal exited"));
    }
    let options = Options::parse(request.uri().query().unwrap_or(""))?;
    let viewer = workspace.viewer(id)?;
    let (mut parts, _) = request.into_parts();
    let incoming = yawc::IncomingUpgrade::from_request_parts(&mut parts, &())
        .await
        .map_err(|_| invalid("invalid websocket upgrade"))?;
    let protocol = yawc::Options::default()
        .with_limits(INPUT + 1, INPUT + 1)
        .with_low_latency_compression()
        .with_compression_threshold(1024)
        .server_no_context_takeover()
        .client_no_context_takeover()
        .with_read_buffer_capacity(4096)
        .with_write_buffer_capacity(4096)
        .with_backpressure_boundary(64 << 10);
    let (response, upgrade) = incoming
        .upgrade(protocol)
        .map_err(|_| invalid("invalid websocket upgrade"))?;
    tokio::spawn(async move {
        if let Ok(socket) = upgrade.await {
            serve(workspace, terminal, socket, options, viewer).await;
        }
    });
    Ok(response.into_response())
}
async fn send<S>(sink: &mut S, frame: Frame) -> bool
where
    S: futures_util::Sink<Frame> + Unpin,
{
    matches!(
        tokio::time::timeout(Duration::from_secs(10), sink.send(frame)).await,
        Ok(Ok(()))
    )
}
async fn serve(
    workspace: Arc<Workspace>,
    terminal: Arc<Terminal>,
    socket: yawc::HttpWebSocket,
    options: Options,
    _viewer: Viewer,
) {
    let (mut sink, mut stream) = socket.split();
    let shared = Arc::new(Shared {
        state: Mutex::new(StreamState {
            pending: 0,
            queue: VecDeque::new(),
            input_total: 0,
            input_ack: 0,
            closed: None,
        }),
        wake: Notify::new(),
        input: Notify::new(),
    });
    let (mut cursor, reset, replay_end, mut size) = terminal.replay(options.since);
    let mut replay = json!({"type":"replay","offset":cursor,"reset":reset});
    if options.flow {
        replay["window"] = json!(WINDOW);
    }
    if options.input {
        replay["inputWindow"] = json!(INPUT);
    }
    if options.geometry {
        replay["size"] = json!(size);
    }
    if !send(&mut sink, Frame::text(replay.to_string())).await {
        return;
    }
    let input_shared = shared.clone();
    let input_term = terminal.clone();
    let input_task = tokio::spawn(async move {
        loop {
            let notified = input_shared.input.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            let bytes = {
                let mut state = input_shared.state.lock().unwrap();
                if state.closed.is_some() {
                    return;
                }
                let count = state.queue.len().min(8192);
                state.queue.drain(..count).collect::<Vec<_>>()
            };
            if bytes.is_empty() {
                notified.await;
                continue;
            }
            if input_term.input(&bytes).await.is_err() {
                input_shared.close(CloseCode::Policy, "terminal input stalled");
                return;
            }
            {
                let mut state = input_shared.state.lock().unwrap();
                state.input_total -= bytes.len();
                state.input_ack += bytes.len();
            }
            input_shared.wake.notify_waiters();
        }
    });
    let read_shared = shared.clone();
    let read_term = terminal.clone();
    let flow = options.flow;
    let read_task = tokio::spawn(async move {
        while let Some(frame) = stream.next().await {
            if frame.opcode() == OpCode::Close {
                break;
            }
            if !matches!(frame.opcode(), OpCode::Text | OpCode::Binary) {
                continue;
            }
            let Ok(message) = serde_json::from_slice::<Message>(frame.payload()) else {
                read_shared.close(CloseCode::Policy, "invalid terminal message");
                return;
            };
            match message.r#type.as_str() {
                "ack" => {
                    let valid = {
                        let mut state = read_shared.state.lock().unwrap();
                        if !flow || message.bytes == 0 || message.bytes > state.pending {
                            false
                        } else {
                            state.pending -= message.bytes;
                            true
                        }
                    };
                    if !valid {
                        read_shared.close(CloseCode::Policy, "invalid terminal acknowledgement");
                        return;
                    }
                    read_shared.wake.notify_waiters();
                }
                "input" => {
                    let bytes = message.data.as_bytes();
                    let valid = {
                        let mut state = read_shared.state.lock().unwrap();
                        if state.input_total + bytes.len() > INPUT {
                            false
                        } else {
                            state.input_total += bytes.len();
                            state.queue.extend(bytes);
                            true
                        }
                    };
                    if !valid {
                        read_shared.close(CloseCode::Policy, "terminal input limit reached");
                        return;
                    }
                    read_shared.input.notify_one();
                }
                "resize" => {
                    if read_term.resize(message.cols, message.rows).is_err() {
                        read_shared.close(CloseCode::Policy, "invalid terminal size");
                        return;
                    }
                }
                _ => {
                    read_shared.close(CloseCode::Policy, "unknown terminal message");
                    return;
                }
            }
        }
        read_shared.close(CloseCode::Normal, "viewer disconnected");
    });
    let mut stop = workspace.stop.subscribe();
    let mut replaying = options.geometry;
    let mut blocked_since = None;
    loop {
        let changed = terminal.changed.notified();
        tokio::pin!(changed);
        changed.as_mut().enable();
        let notified = shared.wake.notified();
        tokio::pin!(notified);
        notified.as_mut().enable();
        let (closed, available, ack) = {
            let mut state = shared.state.lock().unwrap();
            let ack = state.input_ack;
            state.input_ack = 0;
            (
                state.closed,
                if options.flow {
                    WINDOW - state.pending
                } else {
                    64 << 10
                },
                ack,
            )
        };
        if let Some((code, reason)) = closed {
            let _ = send(&mut sink, Frame::close(code, reason)).await;
            break;
        }
        if *stop.borrow() {
            let _ = send(
                &mut sink,
                Frame::close(CloseCode::Away, "workspace stopping"),
            )
            .await;
            break;
        }
        if options.input
            && ack > 0
            && !send(
                &mut sink,
                Frame::text(json!({"type":"input-ack","bytes":ack}).to_string()),
            )
            .await
        {
            break;
        }
        let now = Instant::now();
        if available == 0 {
            let blocked = *blocked_since.get_or_insert(now);
            if now.duration_since(blocked) >= Duration::from_secs(30) {
                let _ = send(
                    &mut sink,
                    Frame::close(CloseCode::Again, "terminal acknowledgement stalled"),
                )
                .await;
                break;
            }
            tokio::select! {_=notified=>{},_=stop.changed()=>{},_=tokio::time::sleep_until(blocked+Duration::from_secs(30))=>{}};
            continue;
        }
        blocked_since = None;
        // Legacy viewers have no parser credits. Bound live lag independently
        // of the shared 32 MiB reconnect ring so a suspended viewer is evicted.
        if !options.flow {
            let (_, _, end, _) = terminal.replay(None);
            if end.saturating_sub(cursor.max(replay_end)) > 512 << 10 {
                let _ = send(
                    &mut sink,
                    Frame::close(CloseCode::Again, "terminal viewer fell behind; reconnect"),
                )
                .await;
                break;
            }
        }
        let mut limit = available.min(64 << 10);
        if replaying && cursor < replay_end {
            limit = limit.min((replay_end - cursor) as usize);
        }
        let Some((bytes, new_size, alive)) = terminal.chunk(cursor, limit, options.geometry) else {
            let _ = send(
                &mut sink,
                Frame::close(CloseCode::Again, "terminal history expired; reconnect"),
            )
            .await;
            break;
        };
        if options.geometry && (size.cols != new_size.cols || size.rows != new_size.rows) {
            if !send(&mut sink,Frame::text(json!({"type":"size","offset":new_size.offset,"cols":new_size.cols,"rows":new_size.rows}).to_string())).await{break;}
            size = new_size;
        }
        if replaying && cursor >= replay_end {
            if !send(&mut sink, Frame::text("{\"type\":\"replay-end\"}")).await {
                break;
            }
            replaying = false;
        }
        if !bytes.is_empty() {
            if options.flow {
                shared.state.lock().unwrap().pending += bytes.len();
            }
            let count = bytes.len();
            if !send(&mut sink, Frame::binary(Bytes::from(bytes))).await {
                break;
            }
            cursor += count as u64;
            continue;
        }
        if !alive {
            let _ = send(
                &mut sink,
                Frame::close(CloseCode::Normal, "terminal exited"),
            )
            .await;
            break;
        }
        tokio::select! {_=changed=>{},_=notified=>{},_=stop.changed()=>{}}
    }
    shared.close(CloseCode::Normal, "viewer closed");
    read_task.abort();
    input_task.abort();
    let _ = read_task.await;
    let _ = input_task.await;
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn stream_query_contract() {
        assert!(Options::parse("flow=1&geometry=1&input=1&since=9007199254740991").is_ok());
        for bad in [
            "since=-1",
            "since=1&since=2",
            "since=9007199254740992",
            "flow=0",
            "geometry=1",
            "input=1&input=1",
        ] {
            assert!(Options::parse(bad).is_err(), "{bad}");
        }
    }
}
