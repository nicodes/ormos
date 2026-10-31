use std::{fs, os::unix::fs::PermissionsExt, path::PathBuf, process::Command, time::Duration};
type Result<T> = std::result::Result<T, Box<dyn std::error::Error>>;
fn prepare() -> Result<(PathBuf, Option<tempfile::TempDir>)> {
    #[cfg(bundled_api)]
    {
        use std::io::{Read, Write};
        use std::os::unix::fs::OpenOptionsExt;
        let directory = tempfile::Builder::new().prefix("ormos-api-").tempdir()?;
        let path = directory.path().join("ormos-api");
        let mut file = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&path)?;
        let compressed = include_bytes!(env!("ORMOS_API_PAYLOAD"));
        let decoder = flate2::read::GzDecoder::new(&compressed[..]);
        let limit = 64 << 20;
        let copied = std::io::copy(&mut decoder.take(limit + 1), &mut file)?;
        if copied > limit {
            return Err("native API payload exceeds its bound".into());
        }
        file.flush()?;
        file.sync_all()?;
        file.set_permissions(fs::Permissions::from_mode(0o700))?;
        Ok((path, Some(directory)))
    }
    #[cfg(not(bundled_api))]
    {
        let path =
            if let Some(path) = std::env::var_os("ORMOS_API_BINARY").filter(|s| !s.is_empty()) {
                PathBuf::from(path)
            } else {
                std::env::current_exe()?
                    .parent()
                    .ok_or("executable directory is unavailable")?
                    .join("ormos-api")
            };
        if !path.is_absolute() {
            return Err("ORMOS_API_BINARY must be absolute".into());
        }
        let valid = fs::metadata(&path)
            .is_ok_and(|info| info.is_file() && info.permissions().mode() & 0o111 != 0);
        if !valid {
            return Err("native Rust API is unavailable; use a release CLI, run mise run build, or set ORMOS_API_BINARY to the built api executable".into());
        }
        Ok((path, None))
    }
}
pub fn run(args: &[std::ffi::OsString], version: &str) -> Result<()> {
    let (path, directory) = prepare()?;
    let mut signals = signal_hook::iterator::Signals::new([
        signal_hook::consts::SIGINT,
        signal_hook::consts::SIGTERM,
    ])?;
    let mut command = Command::new(path);
    command.args(args).args([
        "--app-version",
        version,
        "--parent-pid",
        &std::process::id().to_string(),
    ]);
    let mut child = command.spawn()?;
    // exec has opened the image. The child watches its launcher PID and owns
    // PTY cleanup; unlink extraction now even if the launcher is later killed.
    drop(directory);
    loop {
        for signal in signals.pending() {
            unsafe {
                libc::kill(child.id() as libc::pid_t, signal);
            }
        }
        if let Some(status) = child.try_wait()? {
            if status.success() {
                return Ok(());
            }
            return Err(format!("native Rust API exited with {status}").into());
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}
