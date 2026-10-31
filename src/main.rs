#[cfg(not(any(target_os = "linux", target_os = "macos")))]
compile_error!("The ormos CLI supports Linux and macOS only; the relay SDK is portable.");
#[cfg(any(target_os = "linux", target_os = "macos"))]
mod nativeapi;
#[cfg(any(target_os = "linux", target_os = "macos"))]
fn main() {
    let mut args: Vec<std::ffi::OsString> = std::env::args_os().skip(1).collect();
    if args.is_empty() {
        args.push("ui".into());
    }
    if args[0] == "relay" || args[0] == "--config" {
        eprintln!(
            "error: hosted relay mode is retired; use the local UI with private network access"
        );
        std::process::exit(2);
    }
    match args[0].to_str().unwrap_or("") {
        "--help" if args.len() == 1 => eprint!("{}", include_str!("usage.txt")),
        "--version" if args.len() == 1 => println!("{}", env!("ORMOS_VERSION")),
        "--protocol-version" if args.len() == 1 => {
            println!("{}", ormos_relay::STREAM_FENCE_VERSION)
        }
        "--build-info" if args.len() == 1 => println!(
            "{}",
            serde_json::json!({"version":env!("ORMOS_VERSION"),"commit":env!("ORMOS_COMMIT"),"api_sha256":env!("ORMOS_API_DIGEST"),"bundled_api":cfg!(bundled_api)})
        ),
        "ui" => {
            if let Err(error) = nativeapi::run(&args[1..], env!("ORMOS_VERSION")) {
                eprintln!("error: {error}");
                std::process::exit(2);
            }
        }
        _ => {
            eprintln!("unexpected argument {:?}\n", args[0]);
            eprint!("{}", include_str!("usage.txt"));
            std::process::exit(2);
        }
    }
}
