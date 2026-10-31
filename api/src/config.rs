use crate::error::{Result, fail, invalid};
use serde::Deserialize;
use std::{
    env, fs,
    io::Read,
    net::IpAddr,
    os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
};
#[derive(Clone)]
pub struct Config {
    pub bind: String,
    pub port: u16,
    pub cwd: PathBuf,
    pub hosts: Vec<String>,
    pub config_file: PathBuf,
    pub version: String,
    pub default_config: bool,
    pub parent_pid: Option<u32>,
}
impl Config {
    pub fn parse(args: &[String]) -> Result<Self> {
        let mut bind = "127.0.0.1".to_owned();
        let mut port = 4242u16;
        let mut cwd = None;
        let mut hosts = vec![];
        let mut config_file = None;
        let mut parent_pid = None;
        let mut version = env!("CARGO_PKG_VERSION").to_owned();
        let mut args = args.iter();
        while let Some(argument) = args.next() {
            let (key, value) = if let Some((key, value)) = argument.split_once('=') {
                (key, value.to_owned())
            } else {
                (
                    argument.as_str(),
                    args.next()
                        .ok_or_else(|| invalid("every input must be a flag with a value"))?
                        .clone(),
                )
            };
            match key {
                "--bind" => bind = value,
                "--port" => {
                    port = value
                        .parse()
                        .map_err(|_| invalid("--port must be 1-65535"))?;
                    if port == 0 {
                        return Err(invalid("--port must be 1-65535"));
                    }
                }
                "--cwd" => cwd = Some(PathBuf::from(value)),
                "--hosts" => {
                    hosts = value
                        .split(',')
                        .map(str::trim)
                        .filter(|s| !s.is_empty())
                        .map(str::to_owned)
                        .collect()
                }
                "--config" => config_file = Some(PathBuf::from(value)),
                "--app-version" => version = value,
                "--parent-pid" => {
                    let pid = value
                        .parse::<u32>()
                        .map_err(|_| invalid("invalid launcher PID"))?;
                    if pid == 0 || pid != unsafe { libc::getppid() } as u32 {
                        return Err(invalid("launcher is unavailable"));
                    }
                    parent_pid = Some(pid);
                }
                _ => {
                    return Err(invalid(
                        "unexpected argument; every input must be a supported flag",
                    ));
                }
            }
        }
        if bind != "localhost" && bind.parse::<IpAddr>().is_err() {
            return Err(invalid("--bind must be an IP address or localhost"));
        }
        let home = home()?;
        let cwd = fs::canonicalize(cwd.unwrap_or_else(|| home.clone()))
            .map_err(|_| invalid("cwd does not resolve"))?;
        if !cwd.is_dir() {
            return Err(invalid("cwd is not a directory"));
        }
        let default_config = config_file.is_none();
        let config_file = config_file.unwrap_or_else(|| home.join(".config/ormos/config.json"));
        Ok(Self {
            bind,
            port,
            cwd,
            hosts,
            config_file,
            version,
            default_config,
            parent_pid,
        })
    }
    pub fn directory(&self) -> &Path {
        self.config_file.parent().unwrap_or_else(|| Path::new("."))
    }
    pub fn shell(&self) -> String {
        env::var("SHELL")
            .ok()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| "/bin/bash".into())
    }
    pub fn policy(&self) -> Result<Policy> {
        let file = match fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(self.directory().join("policy.json"))
        {
            Ok(file) => file,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Policy::default()),
            Err(_) => return Err(fail(403, "local policy unreadable")),
        };
        if !file
            .metadata()
            .map_err(|_| fail(403, "local policy unreadable"))?
            .is_file()
        {
            return Err(fail(403, "local policy unreadable"));
        }
        let mut bytes = Vec::new();
        file.take((1 << 20) + 1)
            .read_to_end(&mut bytes)
            .map_err(|_| fail(403, "local policy unreadable"))?;
        if bytes.len() > 1 << 20 {
            return Err(fail(403, "local policy unreadable"));
        }
        serde_json::from_slice(&bytes).map_err(|_| fail(403, "local policy unreadable"))
    }
    pub fn has_config(&self) -> bool {
        if self.default_config
            && let Ok(directory) = fs::OpenOptions::new()
                .read(true)
                .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW)
                .open(self.directory())
            && let Ok(meta) = directory.metadata()
            && meta.uid() == unsafe { libc::geteuid() }
            && meta.permissions().mode() & 0o077 != 0
            && directory
                .set_permissions(fs::Permissions::from_mode(
                    meta.permissions().mode() & !0o077,
                ))
                .is_ok()
        {
            eprintln!("ormos: state directory permissions tightened");
        }
        read_private(&self.config_file)
            .ok()
            .and_then(|bytes| serde_json::from_slice::<serde_json::Value>(&bytes).ok())
            .is_some_and(|v| !v["systemId"].as_str().unwrap_or("").is_empty())
    }
}
fn home() -> Result<PathBuf> {
    env::var_os("HOME")
        .map(PathBuf::from)
        .filter(|p| !p.as_os_str().is_empty())
        .ok_or_else(|| invalid("home directory unavailable"))
}
fn read_private(path: &Path) -> Result<Vec<u8>> {
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_file() {
        return Err(invalid("config is not a regular file"));
    }
    let file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NOFOLLOW | libc::O_NONBLOCK)
        .open(path)?;
    let actual = file.metadata()?;
    if actual.dev() != meta.dev()
        || actual.ino() != meta.ino()
        || actual.nlink() != 1
        || actual.uid() != unsafe { libc::geteuid() }
    {
        return Err(invalid("config ownership is invalid"));
    }
    if actual.permissions().mode() & 0o077 != 0 {
        file.set_permissions(fs::Permissions::from_mode(0o600))?;
        eprintln!(
            "ormos: config permissions tightened; the pairing token may already have been copied; sign out and pair again"
        );
    }
    let mut bytes = Vec::new();
    file.take((1 << 20) + 1).read_to_end(&mut bytes)?;
    if bytes.len() > 1 << 20 {
        return Err(invalid("config exceeds size limit"));
    }
    Ok(bytes)
}
#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Policy {
    pub allowed_roots: Vec<String>,
    pub terminals_disabled: bool,
    pub allowed_ports: Vec<u16>,
    pub denied_ports: Vec<u16>,
}
impl Policy {
    pub fn port(&self, port: u16) -> bool {
        if port == 0 || self.denied_ports.contains(&port) {
            return false;
        }
        if !self.allowed_ports.is_empty() {
            return self.allowed_ports.contains(&port);
        }
        port >= 1024
            && !matches!(
                port,
                1433 | 2375
                    | 2376
                    | 2379
                    | 2380
                    | 3306
                    | 3389
                    | 5432
                    | 5900
                    | 6379
                    | 6443
                    | 9200
                    | 11211
                    | 27017
            )
    }
    pub fn cwd(&self, path: &Path) -> Result<PathBuf> {
        let resolved = fs::canonicalize(path).map_err(|_| invalid("cwd does not resolve"))?;
        if !resolved.is_dir() {
            return Err(invalid("cwd is not a directory"));
        }
        if self.allowed_roots.is_empty() {
            return Ok(resolved);
        }
        for root in &self.allowed_roots {
            let path = if root == "~" {
                home()?
            } else if let Some(rest) = root.strip_prefix("~/") {
                home()?.join(rest)
            } else {
                PathBuf::from(root)
            };
            if let Ok(root) = fs::canonicalize(path)
                && resolved.starts_with(root)
            {
                return Ok(resolved);
            }
        }
        Err(invalid("cwd is outside the policy's allowed roots"))
    }
}
pub fn shell(path: &str) -> Result<()> {
    if ![
        "/bin/sh",
        "/bin/bash",
        "/bin/zsh",
        "/usr/bin/zsh",
        "/usr/bin/bash",
    ]
    .contains(&path)
    {
        return Err(invalid("shell is not on the allowlist"));
    }
    let meta =
        fs::metadata(path).map_err(|_| invalid("shell is not an executable regular file"))?;
    if !meta.is_file() || meta.permissions().mode() & 0o111 == 0 {
        return Err(invalid("shell is not an executable regular file"));
    }
    Ok(())
}
pub fn host(authority: &str, allowed: &[String]) -> bool {
    let parsed = reqwest::Url::parse(&format!("http://{authority}/"));
    if let Ok(url) = parsed
        && url.username().is_empty()
        && url.password().is_none()
    {
        let name = url.host_str().unwrap_or("").trim_matches(['[', ']']);
        if name == "localhost" || name.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback()) {
            return true;
        }
    }
    allowed.iter().any(|h| h.eq_ignore_ascii_case(authority))
}
pub fn origin(origin: &str, authority: &str) -> bool {
    let Ok(url) = reqwest::Url::parse(origin) else {
        return false;
    };
    matches!(url.scheme(), "http" | "https")
        && url.username().is_empty()
        && url.password().is_none()
        && url.query().is_none()
        && url.fragment().is_none()
        && url.path() == "/"
        && url.authority() == authority
        && origin
            .strip_prefix("http://")
            .or_else(|| origin.strip_prefix("https://"))
            == Some(authority)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn private_listener_and_policy_boundaries() {
        assert!(host("127.0.0.1:4242", &[]));
        assert!(!host("attacker.test:4242", &[]));
        assert!(origin("http://127.0.0.1:4242", "127.0.0.1:4242"));
        assert!(!origin("http://evil.test", "127.0.0.1:4242"));
        assert!(shell("/bin/sh -c id").is_err());
        let mut p = Policy::default();
        assert!(!p.port(5432));
        p.allowed_ports = vec![5432];
        assert!(p.port(5432));
        p.denied_ports = vec![5432];
        assert!(!p.port(5432));
    }
}
