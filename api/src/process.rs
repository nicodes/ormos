use crate::error::{Result, fail};
use std::{process::Stdio, time::Duration};
use tokio::{io::AsyncReadExt, process::Command};
// The owner task outlives a cancelled HTTP caller and always kills/reaps its
// direct child. A private process group also releases inherited pipe writers.
pub async fn capture(mut command: Command, limit: usize, budget: Duration) -> Result<Vec<u8>> {
    command
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .stdin(Stdio::null())
        .kill_on_drop(true);
    command.process_group(0);
    let mut child = command
        .spawn()
        .map_err(|_| fail(503, "local command unavailable"))?;
    let pid = child.id().unwrap_or(0) as i32;
    let stdout = child.stdout.take().unwrap();
    let (mut sender, receiver) = tokio::sync::oneshot::channel();
    tokio::spawn(async move {
        let mut reader = tokio::spawn(async move {
            let mut bytes = Vec::new();
            stdout
                .take(limit as u64 + 1)
                .read_to_end(&mut bytes)
                .await
                .map(|_| bytes)
        });
        let mut reader_consumed = false;
        let result = tokio::select! {
            _=sender.closed()=>None,
            _=tokio::time::sleep(budget)=>Some(Err(fail(503,"local command timed out"))),
            result=async{
                let read=(&mut reader).await;reader_consumed=true;let bytes=read.map_err(|_|fail(503,"local command failed"))?.map_err(|_|fail(503,"local command failed"))?;
                if bytes.len()>limit{return Err(fail(503,"local command output limit reached"));}
                let status=child.wait().await?;if !status.success(){return Err(fail(503,"local command failed"));}Ok(bytes)
            }=>Some(result)
        };
        if pid > 0 {
            unsafe { libc::kill(-pid, libc::SIGKILL) };
        }
        let _ = child.kill().await;
        let _ = child.wait().await;
        reader.abort();
        if !reader_consumed {
            let _ = reader.await;
        }
        if let Some(result) = result {
            let _ = sender.send(result);
        }
    });
    receiver
        .await
        .map_err(|_| fail(503, "local command failed"))?
}
#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn bounded_capture_reaps_timed_out_and_cancelled_children() {
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "printf 123456789"]);
        assert!(capture(command, 4, Duration::from_secs(2)).await.is_err());
        let mut command = Command::new("/bin/sh");
        command.args(["-c", "sleep 30"]);
        let start = tokio::time::Instant::now();
        assert!(
            capture(command, 1024, Duration::from_millis(50))
                .await
                .is_err()
        );
        assert!(start.elapsed() < Duration::from_secs(2));
        let path = std::env::temp_dir().join(format!("ormos-command-pid-{}", uuid::Uuid::new_v4()));
        let mut command = Command::new("/bin/sh");
        command
            .args(["-c", "echo $$ > \"$ORMOS_TEST_PID_FILE\"; exec sleep 30"])
            .env("ORMOS_TEST_PID_FILE", &path);
        let task = tokio::spawn(capture(command, 1024, Duration::from_secs(30)));
        let pid = tokio::time::timeout(Duration::from_secs(2), async {
            loop {
                if let Ok(value) = std::fs::read_to_string(&path)
                    && let Ok(pid) = value.trim().parse::<i32>()
                {
                    break pid;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        task.abort();
        let _ = task.await;
        tokio::time::timeout(Duration::from_secs(2), async {
            while unsafe { libc::kill(pid, 0) } == 0 {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
        std::fs::remove_file(path).unwrap();
    }
}
