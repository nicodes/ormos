#![cfg(feature = "transport")]
use futures::{AsyncReadExt, AsyncWriteExt, SinkExt};
use ormos_relay::transport::*;
use std::{io, time::Duration};
use tokio::net::{TcpListener, TcpStream};
use tokio_tungstenite::{accept_async_with_config, client_async_with_config, tungstenite::Message};

async fn exchange(mut session: Session) -> io::Result<()> {
    let mut stream = session.accept().await?;
    let mut length = [0; 4];
    stream.read_exact(&mut length).await?;
    let n = u32::from_be_bytes(length) as usize;
    assert!(n <= 8 << 20);
    let mut bytes = vec![0; n];
    stream.read_exact(&mut bytes).await?;
    stream.write_all(&bytes).await?;
    stream.flush().await?;
    stream.close().await?;
    // Keep the driver alive until the client has received the full response.
    let mut done = session.accept().await?;
    let mut ready = [0; 4];
    done.read_exact(&mut ready).await?;
    assert_eq!(&ready, b"done");
    done.write_all(b"accepted").await?;
    done.flush().await?;
    let mut end = [0];
    let _ = done.read(&mut end).await?;
    Ok(())
}
async fn request(session: Session) -> io::Result<()> {
    let mut stream = session.open().await?;
    let bytes: Vec<u8> = (0..(6 << 20)).map(|i| (i % 251) as u8).collect();
    stream
        .write_all(&(bytes.len() as u32).to_be_bytes())
        .await?;
    stream.write_all(&bytes).await?;
    stream.flush().await?;
    let mut response = vec![0; bytes.len()];
    stream.read_exact(&mut response).await?;
    assert_eq!(response, bytes);
    let mut done = session.open().await?;
    done.write_all(b"done").await?;
    done.flush().await?;
    let mut ack = [0; 8];
    done.read_exact(&mut ack).await?;
    assert_eq!(&ack, b"accepted");
    done.close().await?;
    Ok(())
}
#[tokio::test]
async fn binary_websocket_yamux_moves_multimegabyte_data_and_independent_streams() {
    tokio::time::timeout(Duration::from_secs(20), async {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (socket, _) = listener.accept().await.unwrap();
            let ws = accept_async_with_config(socket, Some(websocket_config()))
                .await
                .unwrap();
            exchange(server_session(NetConn::new(ws))).await.unwrap();
        });
        let socket = TcpStream::connect(address).await.unwrap();
        let (ws, _) =
            client_async_with_config(format!("ws://{address}"), socket, Some(websocket_config()))
                .await
                .unwrap();
        request(client_session(NetConn::new(ws))).await.unwrap();
        server.await.unwrap();
    })
    .await
    .unwrap();
}
#[tokio::test]
async fn binary_adapter_rejects_text_and_handles_message_boundaries() {
    tokio::time::timeout(Duration::from_secs(5), async {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let server = tokio::spawn(async move {
            let (socket, _) = listener.accept().await.unwrap();
            let mut ws = accept_async_with_config(socket, Some(websocket_config()))
                .await
                .unwrap();
            ws.send(Message::Binary(b"ab".as_slice().into()))
                .await
                .unwrap();
            ws.send(Message::Binary(b"cde".as_slice().into()))
                .await
                .unwrap();
            ws.send(Message::Text("invalid".into())).await.unwrap();
        });
        let socket = TcpStream::connect(address).await.unwrap();
        let (ws, _) =
            client_async_with_config(format!("ws://{address}"), socket, Some(websocket_config()))
                .await
                .unwrap();
        let mut io = NetConn::new(ws);
        let mut bytes = [0; 5];
        io.read_exact(&mut bytes).await.unwrap();
        assert_eq!(&bytes, b"abcde");
        assert_eq!(
            io.read(&mut bytes).await.unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );
        server.await.unwrap();
    })
    .await
    .unwrap();
}
// Optional independent, already-built Go SDK peer used during migration acceptance.
// CI always exercises the real Rust WebSocket/Yamux journey above without a Go toolchain.
#[tokio::test]
async fn go_sdk_peer_when_supplied() {
    let Some(peer) = std::env::var_os("ORMOS_GO_RELAY_PEER") else {
        return;
    };
    use std::io::{BufRead, BufReader};
    use std::process::{Command, Stdio};
    let mut child = Command::new(peer).stdout(Stdio::piped()).spawn().unwrap();
    let result = tokio::time::timeout(Duration::from_secs(30), async {
        let mut address = String::new();
        BufReader::new(child.stdout.take().unwrap())
            .read_line(&mut address)
            .unwrap();
        let address = address.trim();
        let socket = TcpStream::connect(address).await.unwrap();
        let (ws, _) =
            client_async_with_config(format!("ws://{address}"), socket, Some(websocket_config()))
                .await
                .unwrap();
        request(client_session(NetConn::new(ws))).await.unwrap();
    })
    .await;
    let _ = child.kill();
    let _ = child.wait();
    result.unwrap();
}
