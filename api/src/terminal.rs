use crate::{
    error::{Result, fail, invalid},
    history::{History, Size},
};
use serde::Serialize;
use std::{
    fs::File,
    os::fd::{AsRawFd, FromRawFd},
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicI32, Ordering},
    },
    time::Duration,
};
use tokio::{
    io::unix::AsyncFd,
    sync::{Mutex as AsyncMutex, Notify, watch},
};
#[derive(Serialize, Clone)]
pub struct Info {
    pub id: String,
    pub shell: String,
    pub cwd: String,
    pub started: String,
    pub alive: bool,
}
struct State {
    info: Info,
    history: History,
    master: Option<Arc<AsyncFd<File>>>,
}
pub struct Terminal {
    state: Mutex<State>,
    pid: i32,
    stopping: watch::Sender<bool>,
    pub done: watch::Receiver<bool>,
    pub changed: Notify,
    writer: AsyncMutex<()>,
    foreground: AtomicI32,
    killed: AtomicBool,
}
impl Terminal {
    pub async fn spawn(shell: &str, cwd: &Path) -> Result<Arc<Self>> {
        use std::os::unix::process::CommandExt;
        let mut master = -1;
        let mut slave = -1;
        let mut size = libc::winsize {
            ws_row: 24,
            ws_col: 80,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        if unsafe {
            libc::openpty(
                &mut master,
                &mut slave,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                &raw mut size,
            )
        } != 0
        {
            return Err(fail(500, "could not open terminal"));
        }
        let master = unsafe { File::from_raw_fd(master) };
        let slave = unsafe { File::from_raw_fd(slave) };
        for file in [&master, &slave] {
            if unsafe { libc::fcntl(file.as_raw_fd(), libc::F_SETFD, libc::FD_CLOEXEC) } < 0 {
                return Err(std::io::Error::last_os_error().into());
            }
        }
        let flags = unsafe { libc::fcntl(master.as_raw_fd(), libc::F_GETFL) };
        if flags < 0
            || unsafe { libc::fcntl(master.as_raw_fd(), libc::F_SETFL, flags | libc::O_NONBLOCK) }
                < 0
        {
            return Err(std::io::Error::last_os_error().into());
        }
        // SAFETY: File owns the open master descriptor. No code replaces or closes it
        // while AsyncFd owns it; Arc clones retain this same File.
        let master = Arc::new(unsafe { AsyncFd::register(master) }.map_err(std::io::Error::from)?);
        let mut command = tokio::process::Command::new(shell);
        command
            .current_dir(cwd)
            .env("TERM", "xterm-256color")
            .stdin(slave.try_clone()?)
            .stdout(slave.try_clone()?)
            .stderr(slave)
            .kill_on_drop(true);
        // Only async-signal-safe syscalls execute between fork and exec. stdin
        // already names the slave PTY after stdio setup in Command.
        unsafe {
            command.as_std_mut().pre_exec(|| {
                if libc::setsid() < 0 || libc::ioctl(0, libc::TIOCSCTTY as _, 0) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let mut child = command
            .spawn()
            .map_err(|_| fail(500, "could not start terminal"))?;
        let pid = child
            .id()
            .ok_or_else(|| fail(500, "terminal process unavailable"))? as i32;
        let (stopping, mut stop) = watch::channel(false);
        let (done, completion) = watch::channel(false);
        let terminal = Arc::new(Self {
            state: Mutex::new(State {
                info: Info {
                    id: format!("t_{}", &uuid::Uuid::new_v4().simple().to_string()[..16]),
                    shell: shell.into(),
                    cwd: cwd.to_string_lossy().into(),
                    started: chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
                    alive: true,
                },
                history: History::default(),
                master: Some(master.clone()),
            }),
            pid,
            stopping,
            done: completion,
            changed: Notify::new(),
            writer: AsyncMutex::new(()),
            foreground: AtomicI32::new(0),
            killed: AtomicBool::new(false),
        });
        let weak = Arc::downgrade(&terminal);
        let (read_stop, mut read_stopping) = watch::channel(false);
        let mut reader = tokio::spawn(async move {
            let mut bytes = vec![0; 8192];
            loop {
                let readiness = tokio::select! {_=read_stopping.changed()=>return,ready=master.readable()=>ready};
                let Ok(mut ready) = readiness else {
                    return;
                };
                let result = ready.try_io(|inner| {
                    let n = unsafe {
                        libc::read(
                            inner.get_ref().as_raw_fd(),
                            bytes.as_mut_ptr().cast(),
                            bytes.len(),
                        )
                    };
                    if n < 0 {
                        Err(std::io::Error::last_os_error())
                    } else {
                        Ok(n as usize)
                    }
                });
                match result {
                    Ok(Ok(0)) => return,
                    Ok(Ok(n)) => {
                        let Some(term) = weak.upgrade() else {
                            return;
                        };
                        term.state.lock().unwrap().history.append(&bytes[..n]);
                        term.changed.notify_waiters();
                    }
                    Ok(Err(error)) if error.kind() == std::io::ErrorKind::Interrupted => continue,
                    Ok(Err(_)) => return,
                    Err(_) => continue,
                }
            }
        });
        let owned = terminal.clone();
        tokio::spawn(async move {
            let requested = tokio::select! {_=stop.changed()=>true,_=child.wait()=>false};
            if requested {
                owned.signal(libc::SIGHUP);
                let _ = tokio::time::timeout(Duration::from_secs(2), child.wait()).await;
                owned.signal(libc::SIGKILL);
                let original = owned.foreground.load(Ordering::Acquire);
                if original > 0 && original != pid {
                    unsafe { libc::kill(-original, libc::SIGKILL) };
                }
                let _ = child.kill().await;
                let _ = child.wait().await;
            } else if tokio::time::timeout(Duration::from_millis(250), &mut reader)
                .await
                .is_err()
            {
                owned.signal(libc::SIGKILL);
            }
            let _ = read_stop.send(true);
            if !reader.is_finished() {
                let _ = reader.await;
            }
            {
                let mut state = owned.state.lock().unwrap();
                state.info.alive = false;
                state.master = None;
            }
            owned.changed.notify_waiters();
            let _ = done.send(true);
        });
        Ok(terminal)
    }
    fn group(&self) -> i32 {
        let state = self.state.lock().unwrap();
        let Some(master) = state.master.as_ref() else {
            return 0;
        };
        let mut group = 0;
        unsafe { libc::ioctl(master.get_ref().as_raw_fd(), libc::TIOCGPGRP, &mut group) };
        group
    }
    fn signal(&self, signal: i32) {
        let foreground = self.group();
        if self.pid > 0 {
            unsafe { libc::kill(-self.pid, signal) };
        }
        if foreground > 0 && foreground != self.pid {
            unsafe { libc::kill(-foreground, signal) };
        }
    }
    pub fn kill(&self) {
        if self.killed.swap(true, Ordering::AcqRel) {
            return;
        }
        if *self.done.borrow() {
            return;
        }
        self.foreground.store(self.group(), Ordering::Release);
        self.state.lock().unwrap().info.alive = false;
        self.signal(libc::SIGHUP);
        let _ = self.stopping.send(true);
        self.changed.notify_waiters();
    }
    pub async fn join(&self) {
        let mut done = self.done.clone();
        while !*done.borrow() {
            if done.changed().await.is_err() {
                break;
            }
        }
    }
    pub fn info(&self) -> Info {
        self.state.lock().unwrap().info.clone()
    }
    pub fn complete(&self) -> bool {
        *self.done.borrow()
    }
    pub fn snapshot(&self) -> String {
        self.state.lock().unwrap().history.snapshot()
    }
    pub fn replay(&self, since: Option<u64>) -> (u64, bool, u64, Size) {
        let state = self.state.lock().unwrap();
        let (offset, reset) = state.history.replay(since);
        (offset, reset, state.history.end, state.history.size(offset))
    }
    pub fn chunk(
        &self,
        offset: u64,
        limit: usize,
        geometry: bool,
    ) -> Option<(Vec<u8>, Size, bool)> {
        let state = self.state.lock().unwrap();
        Some((
            state.history.chunk(offset, limit, geometry)?,
            state.history.size(offset),
            state.info.alive,
        ))
    }
    pub fn resize(&self, cols: u16, rows: u16) -> Result<()> {
        if !(2..=1000).contains(&cols) || !(2..=1000).contains(&rows) {
            return Err(invalid("invalid terminal size"));
        }
        let mut state = self.state.lock().unwrap();
        let master = state
            .master
            .as_ref()
            .ok_or_else(|| fail(410, "terminal exited"))?;
        let size = libc::winsize {
            ws_row: rows,
            ws_col: cols,
            ws_xpixel: 0,
            ws_ypixel: 0,
        };
        if unsafe { libc::ioctl(master.get_ref().as_raw_fd(), libc::TIOCSWINSZ, &size) } < 0 {
            return Err(std::io::Error::last_os_error().into());
        }
        state.history.resize(cols, rows);
        drop(state);
        self.changed.notify_waiters();
        Ok(())
    }
    pub async fn input(&self, bytes: &[u8]) -> Result<()> {
        tokio::time::timeout(Duration::from_secs(5),async{
            let _gate=self.writer.lock().await;let master={let state=self.state.lock().unwrap();if !state.info.alive{return Err(fail(410,"terminal exited"));}state.master.clone().ok_or_else(||fail(410,"terminal exited"))?};let mut stopping=self.stopping.subscribe();let mut offset=0;
            while offset<bytes.len(){if *stopping.borrow(){return Err(fail(410,"terminal exited"));}let readiness=tokio::select!{_=stopping.changed()=>return Err(fail(410,"terminal exited")),ready=master.writable()=>ready?};let mut readiness=readiness;match readiness.try_io(|inner|{let n=unsafe{libc::write(inner.get_ref().as_raw_fd(),bytes[offset..].as_ptr().cast(),bytes.len()-offset)};if n<0{Err(std::io::Error::last_os_error())}else{Ok(n as usize)}}){Ok(Ok(0))=>return Err(fail(503,"terminal input stalled")),Ok(Ok(n))=>offset+=n,Ok(Err(e))if e.kind()==std::io::ErrorKind::Interrupted=>continue,Ok(Err(e))=>return Err(e.into()),Err(_)=>continue}}
            Ok(())
        }).await.map_err(|_|fail(503,"terminal input stalled"))?
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn real_shell_survives_viewer_detach_preserves_final_output_and_reaps_owned_groups() {
        let term = Terminal::spawn("/bin/sh", &std::env::temp_dir())
            .await
            .unwrap();
        term.resize(101, 31).unwrap();
        term.input(b"printf 'ormos-rust-pty-proof\\n'\n")
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), async {
            loop {
                let update = term.changed.notified();
                if term.snapshot().contains("ormos-rust-pty-proof") {
                    break;
                }
                update.await;
            }
        })
        .await
        .unwrap();
        assert!(term.info().alive);
        let (_, _, offset, size) = term.replay(None);
        assert_eq!((size.cols, size.rows), (101, 31));
        assert!(offset > 0);
        term.input(b"printf 'final-tail-proof\\n'; exit\n")
            .await
            .unwrap();
        tokio::time::timeout(Duration::from_secs(5), term.join())
            .await
            .unwrap();
        assert!(!term.info().alive);
        assert!(term.snapshot().contains("final-tail-proof"));
        assert_eq!(unsafe { libc::kill(term.pid, 0) }, -1);
    }
}
