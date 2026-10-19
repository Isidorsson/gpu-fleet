//! Process shutdown requests: SIGINT and SIGTERM on unix, Ctrl-C on Windows.
//! Handlers are installed up front so a failure surfaces at startup.

use std::io;

#[cfg(unix)]
mod imp {
    use super::io;
    use tokio::signal::unix::{Signal, SignalKind, signal};

    pub struct Shutdown {
        interrupt: Signal,
        terminate: Signal,
    }

    impl Shutdown {
        pub fn install() -> io::Result<Self> {
            Ok(Self {
                interrupt: signal(SignalKind::interrupt())?,
                terminate: signal(SignalKind::terminate())?,
            })
        }

        pub async fn requested(mut self) {
            tokio::select! {
                _ = self.interrupt.recv() => {}
                _ = self.terminate.recv() => {}
            }
        }
    }
}

#[cfg(windows)]
mod imp {
    use super::io;
    use tokio::signal::windows::{CtrlC, ctrl_c};

    pub struct Shutdown {
        ctrl_c: CtrlC,
    }

    impl Shutdown {
        pub fn install() -> io::Result<Self> {
            Ok(Self { ctrl_c: ctrl_c()? })
        }

        pub async fn requested(mut self) {
            self.ctrl_c.recv().await;
        }
    }
}

pub use imp::Shutdown;
