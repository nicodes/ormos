use crate::{
    config::Config,
    error::{Result, fail},
    process, server,
};
use axum::{
    Router,
    body::Body,
    extract::{Request, State},
    http::{HeaderMap, Method, StatusCode},
    response::{IntoResponse, Response},
};
use hyper_util::rt::TokioIo;
use serde_json::Value;
use std::{
    collections::BTreeMap,
    net::{IpAddr, SocketAddr},
    process::Stdio,
    sync::Arc,
    time::Duration,
};
use tokio::{
    io::AsyncReadExt,
    net::{TcpListener, TcpStream},
    sync::{Mutex, watch},
};
#[derive(Clone)]
struct Proxy {
    config: Config,
    target: u16,
    scheme: String,
    authority: String,
    stop: watch::Sender<bool>,
    requests: Arc<tokio::sync::Semaphore>,
}
struct Route {
    local: u16,
    public: u16,
    stop: watch::Sender<bool>,
    server: tokio::task::JoinHandle<std::io::Result<()>>,
    session: Option<Session>,
}
impl Drop for Route {
    fn drop(&mut self) {
        let _ = self.stop.send(true);
        if let Some(session) = &self.session {
            let _ = session.stop.send(true);
        }
    }
}
impl Route {
    async fn close(mut self) {
        let _ = self.stop.send(true);
        if let Some(session) = self.session.take() {
            session.close().await;
        }
        let _ = (&mut self.server).await;
    }
}
struct Session {
    stop: watch::Sender<bool>,
    ready: watch::Receiver<bool>,
    done: watch::Receiver<bool>,
}
impl Drop for Session {
    fn drop(&mut self) {
        let _ = self.stop.send(true);
    }
}
impl Session {
    async fn start(scheme: &str, public: u16, local: u16) -> Result<Self> {
        let mut command = tokio::process::Command::new("tailscale");
        command
            .args([
                "serve",
                &format!("--{scheme}={public}"),
                &format!("http://127.0.0.1:{local}"),
            ])
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true);
        command.process_group(0);
        let mut child = command.spawn().map_err(|_| {
            fail(
                503,
                "Install and connect Tailscale on this machine, then refresh.",
            )
        })?;
        let pid = child.id().unwrap_or(0) as i32;
        let mut stdout = child.stdout.take().unwrap();
        let (stop, mut stopping) = watch::channel(false);
        let (ready, readiness) = watch::channel(false);
        let (done, completion) = watch::channel(false);
        tokio::spawn(async move {
            let reader = tokio::spawn(async move {
                let mut tail = Vec::new();
                let mut bytes = [0; 1024];
                while let Ok(n) = stdout.read(&mut bytes).await {
                    if n == 0 {
                        break;
                    }
                    tail.extend_from_slice(&bytes[..n]);
                    if tail
                        .windows(30)
                        .any(|s| s == b"Available within your tailnet:")
                    {
                        let _ = ready.send(true);
                    }
                    if tail.len() > 256 {
                        tail.drain(..tail.len() - 256);
                    }
                }
            });
            let requested = tokio::select! {_=stopping.changed()=>true,_=child.wait()=>false};
            if requested {
                if pid > 0 {
                    unsafe { libc::kill(-pid, libc::SIGINT) };
                }
                if tokio::time::timeout(Duration::from_secs(2), child.wait())
                    .await
                    .is_err()
                {
                    if pid > 0 {
                        unsafe { libc::kill(-pid, libc::SIGKILL) };
                    }
                    let _ = child.kill().await;
                    let _ = child.wait().await;
                }
            }
            reader.abort();
            let _ = reader.await;
            let _ = done.send(true);
        });
        Ok(Self {
            stop,
            ready: readiness,
            done: completion,
        })
    }
    async fn ready(&mut self) -> Result<()> {
        loop {
            if *self.done.borrow() {
                return Err(fail(
                    503,
                    "Tailscale could not open this preview. Check its setup, then refresh.",
                ));
            }
            if *self.ready.borrow() {
                return Ok(());
            }
            tokio::select! {_=self.ready.changed()=>{},_=self.done.changed()=>{}}
        }
    }
    async fn close(mut self) {
        let _ = self.stop.send(true);
        while !*self.done.borrow() {
            if self.done.changed().await.is_err() {
                break;
            }
        }
    }
}
pub struct Previews {
    config: Config,
    routes: Mutex<BTreeMap<String, Route>>,
    stop: watch::Sender<bool>,
}
impl Previews {
    pub fn new(config: Config, stop: watch::Sender<bool>) -> Arc<Self> {
        Arc::new(Self {
            config,
            routes: Mutex::new(BTreeMap::new()),
            stop,
        })
    }
    pub async fn open(self: &Arc<Self>, target: u16, scheme: String, host: String) -> Result<u16> {
        let previews = self.clone();
        let (mut sender, receiver) = tokio::sync::oneshot::channel();
        tokio::spawn(async move {
            let result = tokio::select! {_=sender.closed()=>return,result=tokio::time::timeout(Duration::from_secs(12),previews.open_owned(target,&scheme,&host))=>result.map_err(|_|fail(503,"Tailscale setup did not finish. Check its connection and permissions, then refresh.")).and_then(|r|r)};
            let _ = sender.send(result);
        });
        receiver
            .await
            .map_err(|_| fail(503, "Could not open this port. Refresh to try again."))?
    }
    async fn open_owned(&self, target: u16, scheme: &str, host: &str) -> Result<u16> {
        let mut routes = self.routes.lock().await;
        if *self.stop.borrow() {
            return Err(fail(503, "workspace stopping"));
        }
        let dead: Vec<_> = routes
            .iter()
            .filter(|(_, r)| {
                r.server.is_finished() || r.session.as_ref().is_some_and(|s| *s.done.borrow())
            })
            .map(|(key, _)| key.clone())
            .collect();
        for key in dead {
            if let Some(route) = routes.remove(&key) {
                route.close().await;
            }
        }
        if target == self.config.port || routes.values().any(|r| r.local == target) {
            return Err(fail(403, "Choose an app port, not an ormos preview port."));
        }
        if !self.config.policy()?.port(target) {
            return Err(fail(403, "Local policy does not allow opening this port."));
        }
        let stream = dial(target)
            .await
            .map_err(|_| fail(409, "Start your app in the terminal, then refresh."))?;
        let address = stream.peer_addr()?;
        drop(stream);
        let client = reqwest::Client::builder()
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .timeout(Duration::from_secs(2))
            .build()
            .map_err(|_| fail(503, "preview unavailable"))?;
        if let Ok(response) = client.head(format!("http://{address}/")).send().await
            && response
                .headers()
                .get("x-ormos-workspace")
                .is_some_and(|v| v == "1")
        {
            return Err(fail(
                403,
                "Choose an app port, not an ormos workspace port.",
            ));
        }
        let key = format!("{scheme}:{host}:{target}");
        if let Some(route) = routes.get(&key) {
            return Ok(route.public);
        }
        if routes.len() >= 32 {
            return Err(fail(
                429,
                "Preview port limit reached. Restart ormos to release its preview ports.",
            ));
        }
        let ip = host.parse::<IpAddr>().ok();
        let local = host == "localhost" || ip.is_some_and(|ip| ip.is_loopback());
        let status = if local {
            Value::Null
        } else {
            let mut command = tokio::process::Command::new("tailscale");
            command.args(["serve", "status", "--json"]);
            let bytes = process::capture(command, 1 << 20, Duration::from_secs(3)).await?;
            serde_json::from_slice(&bytes).map_err(|_| {
                fail(
                    503,
                    "Allow ormos's user to manage Tailscale Serve, then refresh.",
                )
            })?
        };
        if !valid_status(&status) {
            return Err(fail(503, "Tailscale Serve status is unreadable."));
        }
        let listener = TcpListener::bind(if local && ip.is_some_and(|ip| ip.is_ipv6()) {
            "[::1]:0"
        } else {
            "127.0.0.1:0"
        })
        .await?;
        let local_port = listener.local_addr()?.port();
        let mut public = local_port;
        if !local {
            let available = |p: u16| {
                p != self.config.port
                    && !uses_port(&status, p)
                    && !routes.values().any(|r| r.local == p || r.public == p)
            };
            if !available(public) {
                public = (20000..=65535)
                    .find(|p| available(*p))
                    .ok_or_else(|| fail(503, "No preview port is available."))?;
            }
        }
        let authority = if host.contains(':') {
            format!("[{host}]:{public}")
        } else {
            format!("{host}:{public}")
        };
        let (stop, stopping) = watch::channel(false);
        let proxy = Arc::new(Proxy {
            config: self.config.clone(),
            target,
            scheme: scheme.into(),
            authority,
            stop: stop.clone(),
            requests: Arc::new(tokio::sync::Semaphore::new(64)),
        });
        let app = Router::new().fallback(forward).with_state(proxy);
        let server = tokio::spawn(server::run(listener, app, stopping));
        let mut route = Route {
            local: local_port,
            public,
            stop,
            server,
            session: None,
        };
        if !local {
            route.session = Some(Session::start(scheme, public, local_port).await?);
            route.session.as_mut().unwrap().ready().await?;
        }
        if *self.stop.borrow() {
            route.close().await;
            return Err(fail(503, "workspace stopping"));
        }
        routes.insert(key, route);
        Ok(public)
    }
    pub async fn close(&self) {
        let mut routes = self.routes.lock().await;
        let owned = std::mem::take(&mut *routes);
        for (_, route) in owned {
            route.close().await;
        }
    }
}
fn valid_status(value: &Value) -> bool {
    if value.is_null() {
        return true;
    }
    let Some(map) = value.as_object() else {
        return false;
    };
    for name in ["TCP", "Web", "AllowFunnel", "Foreground"] {
        let Some(field) = map.get(name).filter(|v| !v.is_null()) else {
            continue;
        };
        let Some(entries) = field.as_object() else {
            return false;
        };
        match name {
            "AllowFunnel" => {
                if !entries.values().all(Value::is_boolean) {
                    return false;
                }
            }
            "Foreground" => {
                if !entries.values().all(valid_status) {
                    return false;
                }
            }
            "Web" => {
                for entry in entries.values().filter(|v| !v.is_null()) {
                    let Some(web) = entry.as_object() else {
                        return false;
                    };
                    if let Some(handlers) = web.get("Handlers").filter(|v| !v.is_null()) {
                        let Some(handlers) = handlers.as_object() else {
                            return false;
                        };
                        for handler in handlers.values().filter(|v| !v.is_null()) {
                            let Some(handler) = handler.as_object() else {
                                return false;
                            };
                            if ["Proxy", "Path", "Text"].iter().any(|key| {
                                handler
                                    .get(*key)
                                    .is_some_and(|v| !v.is_null() && !v.is_string())
                            }) {
                                return false;
                            }
                        }
                    }
                }
            }
            _ => {}
        }
    }
    true
}
fn uses_port(value: &Value, port: u16) -> bool {
    let key = port.to_string();
    if value["TCP"].get(&key).is_some() {
        return true;
    }
    for name in ["Web", "AllowFunnel"] {
        if let Some(items) = value[name].as_object() {
            for (authority, entry) in items {
                if name == "AllowFunnel" && entry != true {
                    continue;
                }
                if authority.rsplit_once(':').is_some_and(|(_, p)| p == key) {
                    return true;
                }
            }
        }
    }
    value["Foreground"]
        .as_object()
        .is_some_and(|items| items.values().any(|v| uses_port(v, port)))
}
pub async fn dial(port: u16) -> std::io::Result<TcpStream> {
    let first = SocketAddr::from(([127, 0, 0, 1], port));
    if let Ok(Ok(stream)) =
        tokio::time::timeout(Duration::from_millis(500), TcpStream::connect(first)).await
    {
        return Ok(stream);
    }
    tokio::time::timeout(
        Duration::from_millis(500),
        TcpStream::connect(SocketAddr::from((std::net::Ipv6Addr::LOCALHOST, port))),
    )
    .await
    .map_err(|_| std::io::Error::new(std::io::ErrorKind::TimedOut, "loopback unavailable"))?
}
fn strip_hop(headers: &mut HeaderMap) {
    let remove: Vec<_> = headers
        .get("connection")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .split(',')
        .filter_map(|s| axum::http::HeaderName::from_bytes(s.trim().as_bytes()).ok())
        .collect();
    for name in remove {
        headers.remove(name);
    }
    for name in [
        "connection",
        "keep-alive",
        "proxy-authenticate",
        "proxy-authorization",
        "te",
        "trailer",
        "transfer-encoding",
        "upgrade",
    ] {
        headers.remove(name);
    }
}
async fn forward(State(proxy): State<Arc<Proxy>>, mut request: Request) -> Response {
    match forward_inner(proxy, &mut request).await {
        Ok(response) => response,
        Err(error) => error.into_response(),
    }
}
async fn forward_inner(proxy: Arc<Proxy>, request: &mut Request) -> Result<Response> {
    let mut permit = Some(
        proxy
            .requests
            .clone()
            .try_acquire_owned()
            .map_err(|_| fail(503, "preview busy"))?,
    );
    if request
        .headers()
        .get("host")
        .and_then(|v| v.to_str().ok())
        .is_none_or(|v| !v.eq_ignore_ascii_case(&proxy.authority))
    {
        return Err(fail(421, "host not allowed"));
    }
    if request.method() == Method::CONNECT {
        return Err(fail(405, "method not allowed"));
    }
    let websocket = request
        .headers()
        .get("upgrade")
        .is_some_and(|v| v.as_bytes().eq_ignore_ascii_case(b"websocket"));
    let origin = format!("{}://{}", proxy.scheme, proxy.authority);
    if let Some(supplied) = request
        .headers()
        .get("origin")
        .and_then(|v| v.to_str().ok())
        && supplied != origin
        && (websocket || !matches!(*request.method(), Method::GET | Method::HEAD))
    {
        return Err(fail(403, "origin not allowed"));
    }
    if !proxy.config.policy()?.port(proxy.target) {
        return Err(fail(403, "Local policy does not allow opening this port."));
    }
    let downstream = websocket.then(|| hyper::upgrade::on(&mut *request));
    let body = std::mem::replace(request.body_mut(), Body::empty());
    let mut upstream = hyper::Request::builder()
        .method(request.method().clone())
        .uri(
            request
                .uri()
                .path_and_query()
                .map(|p| p.as_str())
                .unwrap_or("/"),
        )
        .body(body)
        .map_err(|_| fail(400, "invalid app request"))?;
    *upstream.headers_mut() = request.headers().clone();
    strip_hop(upstream.headers_mut());
    upstream.headers_mut().insert(
        "host",
        format!("localhost:{}", proxy.target).parse().unwrap(),
    );
    if request
        .headers()
        .get("origin")
        .is_some_and(|v| v.as_bytes() == origin.as_bytes())
    {
        upstream.headers_mut().insert(
            "origin",
            format!("http://localhost:{}", proxy.target)
                .parse()
                .unwrap(),
        );
    }
    for name in [
        "forwarded",
        "x-forwarded-host",
        "x-forwarded-proto",
        "x-forwarded-for",
    ] {
        upstream.headers_mut().remove(name);
    }
    if websocket {
        upstream
            .headers_mut()
            .insert("connection", "upgrade".parse().unwrap());
        upstream
            .headers_mut()
            .insert("upgrade", "websocket".parse().unwrap());
    }
    let stream = dial(proxy.target)
        .await
        .map_err(|_| fail(502, "App unavailable. Start your app, then refresh."))?;
    let (mut sender, connection) = hyper::client::conn::http1::handshake(TokioIo::new(stream))
        .await
        .map_err(|_| fail(502, "app unavailable"))?;
    let mut stopping = proxy.stop.subscribe();
    tokio::spawn(async move {
        tokio::select! {_=connection.with_upgrades()=>{},_=stopping.changed()=>{}}
    });
    let mut response = tokio::time::timeout(Duration::from_secs(15), sender.send_request(upstream))
        .await
        .map_err(|_| fail(502, "app unavailable"))?
        .map_err(|_| fail(502, "app unavailable"))?;
    if websocket && response.status() == StatusCode::SWITCHING_PROTOCOLS {
        let upstream = hyper::upgrade::on(&mut response);
        let downstream = downstream.unwrap();
        let mut stopping = proxy.stop.subscribe();
        let tunnel_permit = permit.take().unwrap();
        tokio::spawn(async move {
            let _permit = tunnel_permit;
            tokio::select! {_=async{if let(Ok(up),Ok(down))=tokio::join!(upstream,downstream){let mut up=TokioIo::new(up);let mut down=TokioIo::new(down);let _=tokio::io::copy_bidirectional_with_sizes(&mut up,&mut down,32<<10,32<<10).await;}}=>{},_=stopping.changed()=>{}}
        });
    } else {
        strip_hop(response.headers_mut());
    }
    if let Some(location) = response
        .headers()
        .get("location")
        .and_then(|v| v.to_str().ok())
        && let Some(rewritten) = redirect(location, proxy.target, &proxy.scheme, &proxy.authority)
    {
        response.headers_mut().insert(
            "location",
            rewritten
                .parse()
                .map_err(|_| fail(502, "invalid app redirect"))?,
        );
    }
    let upgraded = websocket && response.status() == StatusCode::SWITCHING_PROTOCOLS;
    if upgraded {
        Ok(response.map(Body::new))
    } else {
        Ok(crate::http::hold(
            response.map(Body::new),
            permit.take().unwrap(),
        ))
    }
}
fn redirect(value: &str, target: u16, scheme: &str, authority: &str) -> Option<String> {
    let raw = if value.starts_with("//") {
        format!("http:{value}")
    } else {
        value.into()
    };
    let mut url = reqwest::Url::parse(&raw).ok()?;
    if url.scheme() != "http"
        || !url.username().is_empty()
        || url.password().is_some()
        || url.port_or_known_default() != Some(target)
        || !matches!(
            url.host_str()?.trim_matches(['[', ']']),
            "localhost" | "127.0.0.1" | "::1"
        )
    {
        return None;
    }
    let public = reqwest::Url::parse(&format!("{scheme}://{authority}")).ok()?;
    url.set_scheme(scheme).ok()?;
    url.set_host(public.host_str()).ok()?;
    url.set_port(public.port()).ok()?;
    Some(url.into())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn tailnet_port_namespace_and_redirects_preserve_other_routes() {
        let cfg = serde_json::json!({"TCP":{"443":{}},"Foreground":{"one":{"AllowFunnel":{"x:8443":true},"Web":{"x:8080":{}}}}});
        for port in [443, 8443, 8080] {
            assert!(uses_port(&cfg, port));
        }
        assert!(!uses_port(&cfg, 5173));
        assert!(valid_status(&cfg));
        assert!(!valid_status(
            &serde_json::json!({"Foreground":{"one":{"AllowFunnel":{"x:8443":"true"}}}})
        ));
        assert!(!valid_status(&serde_json::json!([])));
        assert_eq!(
            redirect(
                "http://localhost:5173/start?q=1",
                5173,
                "https",
                "work.ts.net:20000"
            )
            .unwrap(),
            "https://work.ts.net:20000/start?q=1"
        );
        assert!(redirect("https://other.test:5173/", 5173, "http", "localhost:20000").is_none());
    }
}
