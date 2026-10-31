use crate::error::{Result, fail};
use std::collections::BTreeSet;
#[cfg(target_os = "linux")]
pub async fn listening() -> Result<Vec<u16>> {
    use std::io::Read;
    tokio::task::spawn_blocking(|| {
        let mut ports = BTreeSet::new();
        for path in ["/proc/net/tcp", "/proc/net/tcp6"] {
            let Ok(file) = std::fs::File::open(path) else {
                continue;
            };
            let mut bytes = String::new();
            if file.take(4 << 20).read_to_string(&mut bytes).is_err() {
                continue;
            }
            for line in bytes.lines().skip(1) {
                let fields: Vec<_> = line.split_whitespace().collect();
                if fields.len() < 4 || fields[3] != "0A" {
                    continue;
                }
                let Some((ip, port)) = fields[1].split_once(':') else {
                    continue;
                };
                if matches!(
                    ip,
                    "0100007F"
                        | "00000000"
                        | "00000000000000000000000001000000"
                        | "00000000000000000000000000000000"
                ) && let Ok(port) = u16::from_str_radix(port, 16)
                    && port > 0
                {
                    ports.insert(port);
                }
            }
        }
        Ok(ports.into_iter().collect())
    })
    .await
    .map_err(|_| fail(503, "could not read listening ports"))?
}
#[cfg(target_os = "macos")]
pub async fn listening() -> Result<Vec<u16>> {
    let mut ports = BTreeSet::new();
    let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(2);
    for family in ["inet", "inet6"] {
        let mut command = tokio::process::Command::new("/usr/sbin/netstat");
        command
            .args(["-anl", "-f", family])
            .env_clear()
            .env("LC_ALL", "C")
            .env("LANG", "C");
        let bytes = crate::process::capture(
            command,
            1 << 20,
            deadline.saturating_duration_since(tokio::time::Instant::now()),
        )
        .await?;
        parse_darwin(
            std::str::from_utf8(&bytes).map_err(|_| fail(503, "could not read listening ports"))?,
            &mut ports,
        )?;
    }
    Ok(ports.into_iter().collect())
}
#[cfg(any(target_os = "macos", test))]
fn parse_darwin(text: &str, ports: &mut BTreeSet<u16>) -> Result<()> {
    let mut header = false;
    let mut content = false;
    for line in text.lines() {
        let fields: Vec<_> = line.split_whitespace().collect();
        if fields.is_empty() {
            continue;
        }
        content = true;
        if fields[0] == "Proto" {
            header = fields.len() >= 5 && fields[1] == "Recv-Q" && fields[2] == "Send-Q";
            continue;
        }
        if !fields[0].starts_with("tcp") {
            continue;
        }
        if fields.len() < 6 {
            return Err(fail(503, "could not read listening ports"));
        }
        if fields.last() != Some(&"LISTEN") {
            continue;
        }
        let Some((host, port)) = fields[3].rsplit_once('.') else {
            return Err(fail(503, "could not read listening ports"));
        };
        let port = port
            .parse::<u16>()
            .ok()
            .filter(|p| *p > 0)
            .ok_or_else(|| fail(503, "could not read listening ports"))?;
        if matches!(host, "127.0.0.1" | "*" | "0.0.0.0" | "::1" | "::") {
            ports.insert(port);
        }
    }
    if content && !header {
        return Err(fail(503, "could not read listening ports"));
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn macos_discovery_fails_closed_and_includes_only_reachable_listeners() {
        let mut ports = BTreeSet::new();
        parse_darwin("Proto Recv-Q Send-Q Local Address Foreign Address (state)\ntcp4 0 0 127.0.0.1.4242 *.* LISTEN\ntcp6 0 0 ::1.5173 *.* LISTEN\ntcp4 0 0 10.0.0.2.8080 *.* LISTEN",&mut ports).unwrap();
        assert_eq!(ports.into_iter().collect::<Vec<_>>(), vec![4242, 5173]);
        assert!(parse_darwin("tcp4 0 0 127.0.0.1.80 *.* LISTEN", &mut BTreeSet::new()).is_err());
    }
}
