//! Binary WebSocket byte streams and bounded, independently driven Yamux sessions.
use crate::{MAX_STREAM_WINDOW, MAX_TUNNEL_STREAMS, MAX_TUNNEL_WINDOW_BYTES};
use futures::{AsyncRead, AsyncWrite, Sink, Stream, future::poll_fn, ready};
use std::{
    io,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};
use tokio::{
    sync::{mpsc, oneshot},
    task::JoinHandle,
    time::{Instant, Sleep, timeout},
};
use tokio_tungstenite::{
    WebSocketStream,
    tungstenite::{Message, protocol::WebSocketConfig},
};

pub const ACCEPT_BACKLOG: usize = 64;
pub const STREAM_OPEN_TIMEOUT: Duration = Duration::from_secs(30);
pub const STREAM_CLOSE_TIMEOUT: Duration = Duration::from_secs(120);
const KEEPALIVE: Duration = Duration::from_secs(30);
const KEEPALIVE_TIMEOUT: Duration = Duration::from_secs(60);
pub const MAX_WEBSOCKET_MESSAGE: usize = 2 * MAX_STREAM_WINDOW;

pub fn websocket_config() -> WebSocketConfig {
    WebSocketConfig::default()
        .max_message_size(Some(MAX_WEBSOCKET_MESSAGE))
        .max_frame_size(Some(MAX_WEBSOCKET_MESSAGE))
        .write_buffer_size(0)
        .max_write_buffer_size(MAX_WEBSOCKET_MESSAGE + 1024)
}
fn failed(error: impl std::fmt::Display) -> io::Error {
    io::Error::other(error.to_string())
}
fn closed() -> io::Error {
    io::Error::new(io::ErrorKind::BrokenPipe, "relay session is closed")
}
/// Apply `websocket_config()` when upgrading or connecting the WebSocket.
/// Dropping this adapter cancels its I/O; a quiet peer is probed every 30 seconds.
pub struct NetConn<S> {
    socket: WebSocketStream<S>,
    buffered: Vec<u8>,
    offset: usize,
    ping: Pin<Box<Sleep>>,
    pong_deadline: Option<Pin<Box<Sleep>>>,
}
impl<S> NetConn<S> {
    pub fn new(socket: WebSocketStream<S>) -> Self {
        Self {
            socket,
            buffered: Vec::new(),
            offset: 0,
            ping: Box::pin(tokio::time::sleep(KEEPALIVE)),
            pong_deadline: None,
        }
    }
}
impl<S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin> NetConn<S> {
    fn heartbeat(&mut self, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        if self
            .pong_deadline
            .as_mut()
            .is_some_and(|sleep| sleep.as_mut().poll(cx).is_ready())
        {
            return Poll::Ready(Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "relay keepalive timed out",
            )));
        }
        if self.ping.as_mut().poll(cx).is_ready() {
            if self.pong_deadline.is_none() {
                ready!(Pin::new(&mut self.socket).poll_ready(cx)).map_err(failed)?;
                Pin::new(&mut self.socket)
                    .start_send(Message::Ping(b"ormos".as_slice().into()))
                    .map_err(failed)?;
                self.pong_deadline = Some(Box::pin(tokio::time::sleep(KEEPALIVE_TIMEOUT)));
            }
            self.ping.as_mut().reset(Instant::now() + KEEPALIVE);
        }
        ready!(Pin::new(&mut self.socket).poll_flush(cx)).map_err(failed)?;
        Poll::Ready(Ok(()))
    }
}
impl<S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin> AsyncRead for NetConn<S> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        output: &mut [u8],
    ) -> Poll<io::Result<usize>> {
        if output.is_empty() {
            return Poll::Ready(Ok(0));
        }
        ready!(self.heartbeat(cx))?;
        loop {
            if self.offset < self.buffered.len() {
                let n = output.len().min(self.buffered.len() - self.offset);
                output[..n].copy_from_slice(&self.buffered[self.offset..self.offset + n]);
                self.offset += n;
                if self.offset == self.buffered.len() {
                    self.buffered.clear();
                    self.offset = 0;
                }
                return Poll::Ready(Ok(n));
            }
            match ready!(Pin::new(&mut self.socket).poll_next(cx)) {
                Some(Ok(Message::Binary(bytes))) => {
                    self.buffered = bytes.to_vec();
                    self.offset = 0;
                }
                Some(Ok(Message::Pong(bytes))) => {
                    if bytes == b"ormos".as_slice() {
                        self.pong_deadline = None;
                    }
                }
                Some(Ok(Message::Ping(_))) => {
                    ready!(Pin::new(&mut self.socket).poll_flush(cx)).map_err(failed)?;
                }
                Some(Ok(Message::Close(_))) | None => return Poll::Ready(Ok(0)),
                Some(Ok(_)) => {
                    return Poll::Ready(Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        "relay requires binary WebSocket messages",
                    )));
                }
                Some(Err(error)) => return Poll::Ready(Err(failed(error))),
            }
        }
    }
}
impl<S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin> AsyncWrite for NetConn<S> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        data: &[u8],
    ) -> Poll<io::Result<usize>> {
        if data.is_empty() {
            return Poll::Ready(Ok(0));
        }
        ready!(Pin::new(&mut self.socket).poll_ready(cx)).map_err(failed)?;
        let n = data.len().min(MAX_STREAM_WINDOW);
        Pin::new(&mut self.socket)
            .start_send(Message::Binary(data[..n].to_vec().into()))
            .map_err(failed)?;
        Poll::Ready(Ok(n))
    }
    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.socket).poll_flush(cx).map_err(failed)
    }
    fn poll_close(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.socket).poll_close(cx).map_err(failed)
    }
}
pub fn yamux_config() -> yamux::Config {
    let mut config = yamux::Config::default();
    config.set_max_num_streams(MAX_TUNNEL_STREAMS);
    config.set_max_stream_receive_window(MAX_STREAM_WINDOW as u32);
    // Individual streams and the aggregate budget are both bounded.
    config.set_max_connection_receive_window(Some(MAX_TUNNEL_WINDOW_BYTES));
    config.set_split_send_size(MAX_STREAM_WINDOW);
    config
}
enum Command {
    Open(oneshot::Sender<io::Result<yamux::Stream>>),
    Close(oneshot::Sender<io::Result<()>>),
}
/// Owns a background driver. Keep the session alive while using its streams.
/// `accept()` has a bounded 64-stream backlog; dropping the session cancels all I/O.
pub struct Session {
    commands: mpsc::Sender<Command>,
    incoming: mpsc::Receiver<io::Result<yamux::Stream>>,
    driver: JoinHandle<()>,
}
impl Drop for Session {
    fn drop(&mut self) {
        self.driver.abort();
    }
}
impl Session {
    pub async fn open(&self) -> io::Result<yamux::Stream> {
        timeout(STREAM_OPEN_TIMEOUT, async {
            let (tx, rx) = oneshot::channel();
            self.commands
                .send(Command::Open(tx))
                .await
                .map_err(|_| closed())?;
            rx.await.map_err(|_| closed())?
        })
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "relay stream open timed out"))?
    }
    pub async fn accept(&mut self) -> io::Result<yamux::Stream> {
        self.incoming.recv().await.unwrap_or_else(|| Err(closed()))
    }
    pub async fn close(self) -> io::Result<()> {
        timeout(STREAM_CLOSE_TIMEOUT, async {
            let (tx, rx) = oneshot::channel();
            self.commands
                .send(Command::Close(tx))
                .await
                .map_err(|_| closed())?;
            rx.await.map_err(|_| closed())?
        })
        .await
        .map_err(|_| io::Error::new(io::ErrorKind::TimedOut, "relay stream close timed out"))?
    }
}
pub fn client_session<T: AsyncRead + AsyncWrite + Unpin + Send + 'static>(io: T) -> Session {
    session(io, yamux::Mode::Client)
}
pub fn server_session<T: AsyncRead + AsyncWrite + Unpin + Send + 'static>(io: T) -> Session {
    session(io, yamux::Mode::Server)
}
fn session<T: AsyncRead + AsyncWrite + Unpin + Send + 'static>(
    io: T,
    mode: yamux::Mode,
) -> Session {
    let (commands, mut requests) = mpsc::channel(ACCEPT_BACKLOG);
    let (incoming_tx, incoming) = mpsc::channel(ACCEPT_BACKLOG);
    let driver = tokio::spawn(async move {
        let mut connection = yamux::Connection::new(io, yamux_config(), mode);
        let mut opening: Option<oneshot::Sender<io::Result<yamux::Stream>>> = None;
        let mut closing: Option<oneshot::Sender<io::Result<()>>> = None;
        poll_fn(|cx| {
            if opening.as_ref().is_some_and(oneshot::Sender::is_closed) {
                opening = None;
            }
            if opening.is_none() && closing.is_none() {
                match requests.poll_recv(cx) {
                    Poll::Ready(Some(Command::Open(sender))) => opening = Some(sender),
                    Poll::Ready(Some(Command::Close(sender))) => closing = Some(sender),
                    Poll::Ready(None) => return Poll::Ready(()),
                    Poll::Pending => {}
                }
            }
            if closing.is_some() {
                if let Poll::Ready(result) = connection.poll_close(cx) {
                    let _ = closing.take().unwrap().send(result.map_err(failed));
                    return Poll::Ready(());
                }
            } else {
                if opening.is_some()
                    && let Poll::Ready(result) = connection.poll_new_outbound(cx)
                {
                    let _ = opening.take().unwrap().send(result.map_err(failed));
                    cx.waker().wake_by_ref();
                }
                match connection.poll_next_inbound(cx) {
                    Poll::Ready(Some(Ok(stream))) => {
                        // Dropping excess inbound streams sends a reset rather than allocating a handler.
                        let _ = incoming_tx.try_send(Ok(stream));
                        cx.waker().wake_by_ref();
                    }
                    Poll::Ready(Some(Err(error))) => {
                        let _ = incoming_tx.try_send(Err(failed(error)));
                        return Poll::Ready(());
                    }
                    Poll::Ready(None) => return Poll::Ready(()),
                    Poll::Pending => {}
                }
            }
            Poll::Pending
        })
        .await;
    });
    Session {
        commands,
        incoming,
        driver,
    }
}
