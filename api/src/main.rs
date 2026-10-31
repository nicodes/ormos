use ormos_api::{config::Config, http::App, workspace::Workspace};
#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("ormos: {}", error.message);
        std::process::exit(1);
    }
}
async fn run() -> ormos_api::error::Result<()> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    if args.iter().any(|s| s == "--help" || s == "-h") {
        println!(
            "ormos ui [--bind IP] [--port 4242] [--cwd DIRECTORY] [--hosts HOST:PORT,...]\nA local workspace with terminals and app previews. Defaults to loopback."
        );
        return Ok(());
    }
    let config = Config::parse(&args)?;
    let listener = tokio::net::TcpListener::bind((config.bind.as_str(), config.port)).await?;
    println!("ormos ui on http://{}", listener.local_addr()?);
    let parent_pid = config.parent_pid;
    let workspace = Workspace::new(config);
    let app = App::new(workspace.clone());
    let stop = workspace.stop.subscribe();
    let server = tokio::spawn(ormos_api::server::run(listener, app.router(), stop));
    let mut term = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())?;
    tokio::select! {_=tokio::signal::ctrl_c()=>{},_=term.recv()=>{},_=async{if let Some(pid)=parent_pid{loop{tokio::time::sleep(std::time::Duration::from_millis(250)).await;if unsafe{libc::getppid()}as u32!=pid{break;}}}else{std::future::pending::<()>().await;}}=>{},_ = async {while !server.is_finished(){tokio::time::sleep(std::time::Duration::from_millis(100)).await;}}=>{}}
    app.close().await;
    server
        .await
        .map_err(|_| ormos_api::error::fail(500, "workspace listener failed"))??;
    Ok(())
}
