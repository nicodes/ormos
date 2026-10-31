use crate::{
    config::{self, Config},
    error::{Result, fail},
    terminal::Terminal,
};
use std::{
    collections::BTreeMap,
    path::Path,
    sync::{Arc, Mutex},
};
use tokio::sync::{Notify, OwnedSemaphorePermit, Semaphore};

#[derive(Default)]
struct State {
    terminals: BTreeMap<String, Arc<Terminal>>,
    starting: usize,
    closing: bool,
    viewers: BTreeMap<String, usize>,
    connections: usize,
}
pub struct Workspace {
    pub config: Config,
    state: Mutex<State>,
    changes: Notify,
    pub stop: tokio::sync::watch::Sender<bool>,
    pub requests: Arc<Semaphore>,
}
impl Workspace {
    pub fn new(config: Config) -> Arc<Self> {
        let (stop, _) = tokio::sync::watch::channel(false);
        Arc::new(Self {
            config,
            state: Mutex::new(State::default()),
            changes: Notify::new(),
            stop,
            requests: Arc::new(Semaphore::new(64)),
        })
    }
    pub fn admission(&self) -> Result<OwnedSemaphorePermit> {
        self.requests
            .clone()
            .try_acquire_owned()
            .map_err(|_| fail(503, "workspace busy"))
    }
    pub fn list(&self) -> Vec<crate::terminal::Info> {
        let mut rows: Vec<_> = self
            .state
            .lock()
            .unwrap()
            .terminals
            .values()
            .map(|t| t.info())
            .collect();
        rows.sort_by(|a, b| a.started.cmp(&b.started));
        rows
    }
    pub fn terminal(&self, id: &str) -> Result<Arc<Terminal>> {
        self.state
            .lock()
            .unwrap()
            .terminals
            .get(id)
            .cloned()
            .ok_or_else(|| fail(404, "no such terminal"))
    }
    pub async fn open(self: &Arc<Self>, shell: &str, cwd: &Path) -> Result<Arc<Terminal>> {
        let policy = self.config.policy()?;
        if policy.terminals_disabled {
            return Err(fail(403, "local policy disables terminals"));
        }
        config::shell(shell)?;
        let cwd = policy.cwd(cwd)?;
        {
            let mut state = self.state.lock().unwrap();
            if state.closing {
                return Err(fail(503, "workspace is stopping"));
            }
            state.terminals.retain(|_, t| !t.complete());
            if state.terminals.len() + state.starting >= 8 {
                return Err(fail(429, "terminal limit reached"));
            }
            state.starting += 1;
        }
        // A task owns every admitted spawn through registration, even when the
        // HTTP caller disconnects. Shutdown waits for these tasks before reaping.
        let workspace = self.clone();
        let shell = shell.to_owned();
        tokio::spawn(async move {
            let terminal = Terminal::spawn(&shell, &cwd).await;
            let closing = {
                let mut state = workspace.state.lock().unwrap();
                state.starting -= 1;
                if let Ok(t) = &terminal {
                    state.terminals.insert(t.info().id, t.clone());
                }
                state.closing
            };
            workspace.changes.notify_waiters();
            if closing && let Ok(t) = &terminal {
                t.kill();
            }
            terminal
        })
        .await
        .map_err(|_| fail(500, "terminal task failed"))?
    }
    pub fn viewer(self: &Arc<Self>, id: &str) -> Result<Viewer> {
        let mut state = self.state.lock().unwrap();
        if state.closing {
            return Err(fail(503, "workspace is stopping"));
        }
        if state.connections >= 64 || state.viewers.get(id).copied().unwrap_or(0) >= 8 {
            return Err(fail(429, "terminal attachment limit reached"));
        }
        state.connections += 1;
        *state.viewers.entry(id.into()).or_default() += 1;
        Ok(Viewer {
            workspace: self.clone(),
            id: id.into(),
        })
    }
    pub async fn shutdown(&self) {
        {
            self.state.lock().unwrap().closing = true;
        }
        let _ = self.stop.send(true);
        loop {
            let notified = self.changes.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            if self.state.lock().unwrap().starting == 0 {
                break;
            }
            notified.await;
        }
        let terminals: Vec<_> = self
            .state
            .lock()
            .unwrap()
            .terminals
            .values()
            .cloned()
            .collect();
        for t in &terminals {
            t.kill();
        }
        for t in terminals {
            t.join().await;
        }
    }
}
pub struct Viewer {
    workspace: Arc<Workspace>,
    id: String,
}
impl Drop for Viewer {
    fn drop(&mut self) {
        let mut state = self.workspace.state.lock().unwrap();
        state.connections -= 1;
        if let Some(count) = state.viewers.get_mut(&self.id) {
            *count -= 1;
            if *count == 0 {
                state.viewers.remove(&self.id);
            }
        }
    }
}
