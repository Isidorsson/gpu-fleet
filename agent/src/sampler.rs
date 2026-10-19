//! Reads the backend on a fixed interval and publishes the latest snapshot.
//! Scrapes only read the published snapshot, so a slow scrape never delays
//! sampling and a slow backend never delays a scrape.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::watch;
use tokio::task;
use tokio::time::{self, MissedTickBehavior};

use crate::gpu::{BackendError, GpuBackend, GpuSample, Snapshot, UnixSeconds};
use crate::metrics::Counters;

pub type SnapshotReceiver = watch::Receiver<Arc<Snapshot>>;

pub struct Sampler<B, C> {
    backend: Arc<B>,
    clock: C,
    counters: Counters,
    publisher: watch::Sender<Arc<Snapshot>>,
}

impl<B, C> Sampler<B, C>
where
    B: GpuBackend,
    C: Fn() -> UnixSeconds,
{
    pub fn new(backend: B, clock: C, counters: Counters) -> (Self, SnapshotReceiver) {
        let (publisher, receiver) = watch::channel(Arc::new(Snapshot::default()));
        let sampler = Self {
            backend: Arc::new(backend),
            clock,
            counters,
            publisher,
        };
        (sampler, receiver)
    }

    pub async fn run(self, interval: Duration) {
        let mut ticker = time::interval(interval);
        ticker.set_missed_tick_behavior(MissedTickBehavior::Skip);
        loop {
            ticker.tick().await;
            self.sample_once().await;
        }
    }

    /// Takes one sample. On failure the previous snapshot stays published.
    pub async fn sample_once(&self) {
        let at = (self.clock)();
        match self.read_backend(at).await {
            Ok(gpus) => {
                self.publisher
                    .send_replace(Arc::new(Snapshot { taken_at: at, gpus }));
                self.counters.samples.inc();
            }
            Err(e) => {
                self.counters.errors.inc();
                eprintln!("gpu-agent: sample failed: {e}");
            }
        }
    }

    async fn read_backend(&self, at: UnixSeconds) -> Result<Vec<GpuSample>, BackendError> {
        let backend = Arc::clone(&self.backend);
        task::spawn_blocking(move || backend.sample(at))
            .await
            .map_err(|e| BackendError(format!("backend task failed: {e}")))?
    }
}

pub fn system_clock() -> UnixSeconds {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |since_epoch| since_epoch.as_secs())
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};

    use super::*;
    use crate::fake::FakeBackend;

    struct Flaky {
        failing: Arc<AtomicBool>,
        inner: FakeBackend,
    }

    impl GpuBackend for Flaky {
        fn sample(&self, at: UnixSeconds) -> Result<Vec<GpuSample>, BackendError> {
            if self.failing.load(Ordering::SeqCst) {
                return Err(BackendError("nvml unavailable".to_owned()));
            }
            self.inner.sample(at)
        }
    }

    struct Panicking;

    impl GpuBackend for Panicking {
        fn sample(&self, _at: UnixSeconds) -> Result<Vec<GpuSample>, BackendError> {
            panic!("backend exploded");
        }
    }

    fn ticking_clock(start: UnixSeconds) -> impl Fn() -> UnixSeconds {
        let now = AtomicU64::new(start);
        move || now.fetch_add(5, Ordering::SeqCst)
    }

    #[tokio::test]
    async fn publishes_each_successful_sample() {
        let counters = Counters::default();
        let backend = FakeBackend::new(2, "node-a");
        let (sampler, snapshots) = Sampler::new(backend, ticking_clock(100), counters.clone());
        assert_eq!(*snapshots.borrow().as_ref(), Snapshot::default());

        sampler.sample_once().await;
        assert_eq!(snapshots.borrow().taken_at, 100);
        assert_eq!(snapshots.borrow().gpus.len(), 2);

        sampler.sample_once().await;
        assert_eq!(snapshots.borrow().taken_at, 105);
        assert_eq!(counters.samples.get(), 2);
        assert_eq!(counters.errors.get(), 0);
    }

    #[tokio::test]
    async fn backend_error_counts_and_keeps_previous_snapshot() {
        let failing = Arc::new(AtomicBool::new(false));
        let backend = Flaky {
            failing: Arc::clone(&failing),
            inner: FakeBackend::new(2, "node-a"),
        };
        let counters = Counters::default();
        let (sampler, snapshots) = Sampler::new(backend, ticking_clock(100), counters.clone());

        sampler.sample_once().await;
        let before = Arc::clone(&snapshots.borrow());

        failing.store(true, Ordering::SeqCst);
        sampler.sample_once().await;
        sampler.sample_once().await;

        assert_eq!(**snapshots.borrow(), *before);
        assert_eq!(counters.samples.get(), 1);
        assert_eq!(counters.errors.get(), 2);

        failing.store(false, Ordering::SeqCst);
        sampler.sample_once().await;
        assert_eq!(snapshots.borrow().taken_at, 115);
        assert_eq!(counters.samples.get(), 2);
    }

    #[tokio::test]
    async fn first_failure_leaves_the_empty_snapshot() {
        let backend = Flaky {
            failing: Arc::new(AtomicBool::new(true)),
            inner: FakeBackend::new(2, "node-a"),
        };
        let counters = Counters::default();
        let (sampler, snapshots) = Sampler::new(backend, ticking_clock(100), counters.clone());

        sampler.sample_once().await;

        assert_eq!(**snapshots.borrow(), Snapshot::default());
        assert_eq!(counters.errors.get(), 1);
    }

    #[test]
    fn system_clock_is_after_2020() {
        assert!(system_clock() > 1_577_836_800);
    }

    #[tokio::test]
    async fn panicking_backend_counts_as_an_error() {
        let counters = Counters::default();
        let (sampler, snapshots) = Sampler::new(Panicking, ticking_clock(100), counters.clone());

        sampler.sample_once().await;

        assert_eq!(**snapshots.borrow(), Snapshot::default());
        assert_eq!(counters.errors.get(), 1);
    }
}
