use axum::Router;
use hyper_util::{
    rt::{TokioIo, TokioTimer},
    service::TowerToHyperService,
};
use std::time::Duration;
use tokio::{net::TcpListener, sync::watch, task::JoinSet};
pub async fn run(
    listener: TcpListener,
    app: Router,
    mut stop: watch::Receiver<bool>,
) -> std::io::Result<()> {
    let mut connections = JoinSet::new();
    loop {
        if *stop.borrow() {
            break;
        }
        tokio::select! {
            _=stop.changed()=>break,
            Some(_)=connections.join_next()=>{},
            accepted=listener.accept()=>{let(stream,_)=accepted?;
                if connections.len()>=256{drop(stream);continue;}
                let app=app.clone();connections.spawn(async move{
                    let _=stream.set_nodelay(true);
                    let _=hyper::server::conn::http1::Builder::new().timer(TokioTimer::new()).header_read_timeout(Duration::from_secs(10)).max_buf_size(64<<10).serve_connection(TokioIo::new(stream),TowerToHyperService::new(app)).with_upgrades().await;
                });
            }
        }
    }
    connections.abort_all();
    while connections.join_next().await.is_some() {}
    Ok(())
}
