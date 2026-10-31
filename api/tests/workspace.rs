use axum::extract::FromRequestParts;
use axum::{Json, Router, extract::Request, response::IntoResponse};
use futures_util::{SinkExt, StreamExt};
use ormos_api::{config::Config, http::App, workspace::Workspace};
use serde_json::{Value, json};
use std::{sync::Arc, time::Duration};
use yawc::{Frame, OpCode};
struct Fixture {
    app: Arc<App>,
    url: String,
    directory: std::path::PathBuf,
    server: tokio::task::JoinHandle<std::io::Result<()>>,
    client: reqwest::Client,
}
impl Fixture {
    async fn new() -> Self {
        let directory =
            std::env::temp_dir().join(format!("ormos-rust-test-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir(&directory).unwrap();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let config = Config::parse(&[
            "--port".into(),
            address.port().to_string(),
            "--cwd".into(),
            directory.to_string_lossy().into(),
            "--config".into(),
            directory.join("config.json").to_string_lossy().into(),
        ])
        .unwrap();
        let workspace = Workspace::new(config);
        let app = App::new(workspace.clone());
        let server = tokio::spawn(ormos_api::server::run(
            listener,
            app.router(),
            workspace.stop.subscribe(),
        ));
        Self {
            app,
            url: format!("http://{address}"),
            directory,
            server,
            client: reqwest::Client::builder()
                .no_proxy()
                .timeout(Duration::from_secs(10))
                .build()
                .unwrap(),
        }
    }
    async fn post(&self, path: &str, value: Value) -> reqwest::Response {
        self.client
            .post(format!("{}{path}", self.url))
            .header("origin", &self.url)
            .json(&value)
            .send()
            .await
            .unwrap()
    }
    async fn ws(&self, id: &str, query: &str) -> yawc::TcpWebSocket {
        yawc::WebSocket::connect(
            format!(
                "{}/api/terminal/{id}/ws?{query}",
                self.url.replace("http:", "ws:")
            )
            .parse()
            .unwrap(),
        )
        .with_request(yawc::HttpRequestBuilder::new().header("Origin", &self.url))
        .with_options(
            yawc::Options::default()
                .with_low_latency_compression()
                .server_no_context_takeover()
                .client_no_context_takeover(),
        )
        .await
        .unwrap()
    }
    async fn close(self) {
        self.app.close().await;
        self.server.await.unwrap().unwrap();
        std::fs::remove_dir_all(self.directory).unwrap();
    }
}
async fn next(socket: &mut yawc::TcpWebSocket) -> Frame {
    tokio::time::timeout(Duration::from_secs(5), socket.next())
        .await
        .unwrap()
        .expect("websocket closed")
}
async fn text(socket: &mut yawc::TcpWebSocket) -> Value {
    let frame = next(socket).await;
    assert_eq!(frame.opcode(), OpCode::Text);
    serde_json::from_slice(frame.payload()).unwrap()
}
#[tokio::test]
async fn real_http_shell_compressed_stream_resume_policy_and_shutdown() {
    let fixture = Fixture::new().await;
    let wrong = fixture
        .client
        .get(format!("{}/api/system", fixture.url))
        .header("host", "foreign.invalid")
        .send()
        .await
        .unwrap();
    assert_eq!(wrong.status(), 421);
    let foreign = fixture
        .client
        .post(format!("{}/api/action", fixture.url))
        .header("origin", "https://foreign.invalid")
        .json(&json!({"action":"open"}))
        .send()
        .await
        .unwrap();
    assert_eq!(foreign.status(), 403);
    assert_eq!(
        fixture
            .post("/api/action", json!({"action":"exec"}))
            .await
            .status(),
        403
    );
    assert_eq!(
        fixture
            .post(
                "/api/action",
                json!({"action":"open","shell":"/bin/sh -c id"})
            )
            .await
            .status(),
        400
    );
    assert_eq!(
        fixture
            .post("/api/action", json!({"action":"open","extra":true}))
            .await
            .status(),
        400
    );
    let result = fixture
        .post("/api/action", json!({"action":"open","shell":"/bin/sh"}))
        .await;
    assert_eq!(result.status(), 200);
    let id = result.json::<Value>().await.unwrap()["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let mut socket = fixture.ws(&id, "flow=1&input=1&geometry=1").await;
    let replay = text(&mut socket).await;
    assert_eq!(replay["type"], "replay");
    assert_eq!(replay["reset"], true);
    assert_eq!(replay["window"], 262144);
    assert_eq!(replay["inputWindow"], 32768);
    socket
        .send(Frame::text(
            json!({"type":"resize","cols":101,"rows":31}).to_string(),
        ))
        .await
        .unwrap();
    socket
        .send(Frame::text(
            json!({"type":"input","data":"printf 'rust-stream-proof\\n'\n"}).to_string(),
        ))
        .await
        .unwrap();
    let mut cursor = replay["offset"].as_u64().unwrap();
    let mut output = Vec::new();
    let mut resized = false;
    let mut input_ack = false;
    while !(output.windows(17).any(|s| s == b"rust-stream-proof") && resized && input_ack) {
        let frame = next(&mut socket).await;
        if frame.opcode() == OpCode::Binary {
            cursor += frame.payload().len() as u64;
            output.extend_from_slice(frame.payload());
            socket
                .send(Frame::text(
                    json!({"type":"ack","bytes":frame.payload().len()}).to_string(),
                ))
                .await
                .unwrap();
        } else if frame.opcode() == OpCode::Text {
            let event: Value = serde_json::from_slice(frame.payload()).unwrap();
            if event["type"] == "size" {
                resized = event["cols"] == 101 && event["rows"] == 31;
            }
            if event["type"] == "input-ack" {
                input_ack = true;
            }
        }
    }
    drop(socket);
    let terminal = fixture.app.workspace.terminal(&id).unwrap();
    assert!(terminal.info().alive);
    let mut socket = fixture
        .ws(&id, &format!("flow=1&input=1&geometry=1&since={cursor}"))
        .await;
    let replay = text(&mut socket).await;
    assert_eq!(replay["reset"], false);
    assert_eq!(replay["offset"], cursor);
    assert_eq!(replay["size"]["cols"], 101);
    // Reject invalid ACKs without killing the owned shell.
    socket
        .send(Frame::text("{\"type\":\"ack\",\"bytes\":999999}"))
        .await
        .unwrap();
    loop {
        let frame = next(&mut socket).await;
        if frame.opcode() == OpCode::Close {
            assert_eq!(frame.close_code(), Some(yawc::close::CloseCode::Policy));
            break;
        }
    }
    assert!(terminal.info().alive);
    std::fs::write(
        fixture.directory.join("policy.json"),
        b"{\"terminalsDisabled\":true}",
    )
    .unwrap();
    assert_eq!(
        fixture
            .post("/api/action", json!({"action":"open"}))
            .await
            .status(),
        403
    );
    assert_eq!(
        fixture
            .post("/api/action", json!({"action":"kill","id":id}))
            .await
            .status(),
        200
    );
    tokio::time::timeout(Duration::from_secs(5), terminal.join())
        .await
        .unwrap();
    assert!(!terminal.info().alive);
    fixture.close().await;
}
#[tokio::test]
async fn preview_forwards_only_fixed_loopback_app_and_rechecks_policy() {
    let fixture = Fixture::new().await;
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let target = listener.local_addr().unwrap().port();
    let route=Router::new().fallback(move |request:Request|async move{
        if request.uri().path()=="/socket" {
            let(mut parts,_)=request.into_parts();let incoming=yawc::IncomingUpgrade::from_request_parts(&mut parts,&()).await.unwrap();let(response,upgrade)=incoming.upgrade(yawc::Options::default()).unwrap();tokio::spawn(async move{if let Ok(mut socket)=upgrade.await{while let Some(frame)=socket.next().await{if matches!(frame.opcode(),OpCode::Text|OpCode::Binary)&&socket.send(frame).await.is_err(){break;}}}});return response.into_response();
        }
        if request.uri().path()=="/redirect"{return (axum::http::StatusCode::FOUND,[("location",format!("http://localhost:{target}/landed"))]).into_response();}Json(json!({"host":request.headers().get("host").unwrap().to_str().unwrap(),"origin":request.headers().get("origin").and_then(|v|v.to_str().ok()),"forwarded":request.headers().contains_key("x-forwarded-for")})).into_response()});
    let (stop, stopping) = tokio::sync::watch::channel(false);
    let upstream = tokio::spawn(ormos_api::server::run(listener, route, stopping));
    let response = fixture
        .post("/api/preview", json!({"port":target,"scheme":"http"}))
        .await;
    assert_eq!(response.status(), 200);
    let port = response.json::<Value>().await.unwrap()["port"]
        .as_u64()
        .unwrap();
    let public = format!("http://127.0.0.1:{port}");
    let response = fixture
        .client
        .post(&public)
        .header("origin", &public)
        .header("x-forwarded-for", "secret")
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 200);
    let report = response.json::<Value>().await.unwrap();
    assert_eq!(report["host"], format!("localhost:{target}"));
    assert_eq!(report["origin"], format!("http://localhost:{target}"));
    assert_eq!(report["forwarded"], false);
    assert_eq!(
        fixture
            .client
            .get(&public)
            .header("host", "foreign.invalid")
            .send()
            .await
            .unwrap()
            .status(),
        421
    );
    assert_eq!(
        fixture
            .client
            .post(&public)
            .header("origin", "http://foreign.invalid")
            .send()
            .await
            .unwrap()
            .status(),
        403
    );
    let no_redirect = reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .unwrap();
    let redirect = no_redirect
        .get(format!("{public}/redirect"))
        .send()
        .await
        .unwrap();
    assert_eq!(redirect.headers()["location"], format!("{public}/landed"));
    let mut socket = yawc::WebSocket::connect(
        format!("{}/socket", public.replace("http:", "ws:"))
            .parse()
            .unwrap(),
    )
    .with_request(yawc::HttpRequestBuilder::new().header("Origin", &public))
    .await
    .unwrap();
    socket
        .send(Frame::binary("preview-upgrade-proof"))
        .await
        .unwrap();
    let echoed = next(&mut socket).await;
    assert_eq!(echoed.payload().as_ref(), b"preview-upgrade-proof");
    drop(socket);
    std::fs::write(
        fixture.directory.join("policy.json"),
        format!("{{\"deniedPorts\":[{target}]}}"),
    )
    .unwrap();
    assert_eq!(
        fixture.client.get(&public).send().await.unwrap().status(),
        403
    );
    fixture.close().await;
    let _ = stop.send(true);
    upstream.await.unwrap().unwrap();
    assert!(
        tokio::net::TcpStream::connect(format!("127.0.0.1:{port}"))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn terminal_capacity_and_attachment_limits_remain_owned_until_reap() {
    let fixture = Fixture::new().await;
    let mut terminals = Vec::new();
    for _ in 0..8 {
        let response = fixture
            .post("/api/action", json!({"action":"open","shell":"/bin/sh"}))
            .await;
        assert_eq!(response.status(), 200);
        let id = response.json::<Value>().await.unwrap()["id"]
            .as_str()
            .unwrap()
            .to_owned();
        terminals.push(fixture.app.workspace.terminal(&id).unwrap());
    }
    assert_eq!(
        fixture
            .post("/api/action", json!({"action":"open","shell":"/bin/sh"}))
            .await
            .status(),
        429
    );
    let id = terminals[0].info().id;
    let mut sockets = Vec::new();
    for _ in 0..8 {
        let mut socket = fixture.ws(&id, "flow=1").await;
        assert_eq!(text(&mut socket).await["type"], "replay");
        sockets.push(socket);
    }
    let endpoint = format!("{}/api/terminal/{id}/ws", fixture.url);
    let denied = fixture
        .client
        .get(&endpoint)
        .header("origin", &fixture.url)
        .header("connection", "Upgrade")
        .header("upgrade", "websocket")
        .header("sec-websocket-version", "13")
        .header("sec-websocket-key", "dGhlIHNhbXBsZSBub25jZQ==")
        .send()
        .await
        .unwrap();
    assert_eq!(denied.status(), 429);
    assert_eq!(
        fixture
            .post("/api/action", json!({"action":"kill","id":id}))
            .await
            .status(),
        200
    );
    terminals[0].join().await;
    let replacement = fixture
        .post("/api/action", json!({"action":"open","shell":"/bin/sh"}))
        .await;
    assert_eq!(replacement.status(), 200);
    drop(sockets);
    fixture.close().await;
    for terminal in terminals {
        assert!(terminal.complete());
    }
}

#[tokio::test]
async fn websocket_deflate_and_loopback_origin_are_enforced_before_upgrade() {
    let fixture = Fixture::new().await;
    let response = fixture
        .post("/api/action", json!({"action":"open","shell":"/bin/sh"}))
        .await;
    let id = response.json::<Value>().await.unwrap()["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let endpoint = format!("{}/api/terminal/{id}/ws", fixture.url);
    let request = || {
        fixture
            .client
            .get(&endpoint)
            .header("connection", "Upgrade")
            .header("upgrade", "websocket")
            .header("sec-websocket-version", "13")
            .header("sec-websocket-key", "dGhlIHNhbXBsZSBub25jZQ==")
            .header(
                "sec-websocket-extensions",
                "permessage-deflate; client_max_window_bits",
            )
    };
    assert_eq!(request().send().await.unwrap().status(), 403);
    assert_eq!(
        request()
            .header("origin", "http://foreign.invalid")
            .send()
            .await
            .unwrap()
            .status(),
        403
    );
    let response = request()
        .header("origin", &fixture.url)
        .send()
        .await
        .unwrap();
    assert_eq!(response.status(), 101);
    let extension = response.headers()["sec-websocket-extensions"]
        .to_str()
        .unwrap();
    assert!(extension.contains("permessage-deflate"));
    assert!(extension.contains("server_no_context_takeover"));
    assert!(extension.contains("client_no_context_takeover"));
    drop(response);
    fixture.close().await;
}

#[tokio::test]
async fn parser_credit_window_blocks_output_until_ack_without_blocking_resize() {
    let fixture = Fixture::new().await;
    let result = fixture
        .post("/api/action", json!({"action":"open","shell":"/bin/sh"}))
        .await;
    let id = result.json::<Value>().await.unwrap()["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let terminal = fixture.app.workspace.terminal(&id).unwrap();
    let mut socket = fixture.ws(&id, "flow=1&geometry=1").await;
    assert_eq!(text(&mut socket).await["window"], 262144);
    socket
        .send(Frame::text(
            json!({"type":"input","data":"head -c 1048576 /dev/zero\n"}).to_string(),
        ))
        .await
        .unwrap();
    let mut received = 0;
    while received < 262144 {
        let frame = next(&mut socket).await;
        if frame.opcode() == OpCode::Binary {
            received += frame.payload().len();
        }
    }
    assert_eq!(received, 262144);
    socket
        .send(Frame::text(
            "{\"type\":\"resize\",\"cols\":137,\"rows\":43}",
        ))
        .await
        .unwrap();
    tokio::time::timeout(Duration::from_secs(2), async {
        loop {
            let (_, _, end, _) = terminal.replay(None);
            if terminal.replay(Some(end)).3.cols == 137 {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    // No output credit remains; the socket reader still applies the resize.
    loop {
        match tokio::time::timeout(Duration::from_millis(50), socket.next()).await {
            Err(_) => break,
            Ok(Some(frame)) => assert_ne!(frame.opcode(), OpCode::Binary),
            Ok(None) => panic!("credit flow unexpectedly disconnected"),
        }
    }
    socket
        .send(Frame::text("{\"type\":\"ack\",\"bytes\":65536}"))
        .await
        .unwrap();
    loop {
        let frame = next(&mut socket).await;
        if frame.opcode() == OpCode::Binary {
            assert!(frame.payload().len() <= 65536);
            break;
        }
    }
    assert!(terminal.info().alive);
    drop(socket);
    fixture.close().await;
}
