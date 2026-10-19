//! Simulated GPUs for machines without NVIDIA hardware. Readings are a pure
//! function of (gpu index, time) so graphs move and tests can pin time. XIDs
//! are the one injectable input, through a shared `XidInjector` handle.

use std::fmt;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use crate::gpu::{BackendError, GpuBackend, GpuSample, UnixSeconds};

pub const MODEL_NAME: &str = "NVIDIA H100 80GB HBM3";

const TEMP_MIN_C: u32 = 40;
const TEMP_MAX_C: u32 = 80;
const TEMP_PERIOD_SECS: u32 = 240;

const POWER_MIN_DECIWATTS: u32 = 1_000;
const POWER_MAX_DECIWATTS: u32 = 7_000;
const POWER_PERIOD_SECS: u32 = 120;
const DECIWATTS_PER_WATT: f64 = 10.0;

const GPU_PHASE_STEP_SECS: u64 = 17;
const PERMILLE_MAX: u32 = 1_000;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InjectedXid {
    pub gpu: usize,
    pub code: u32,
}

/// Shared handle to the XIDs a `FakeBackend` reports. Clones see the same
/// state, so the HTTP layer can inject while the sampler reads, and the
/// `GpuBackend` trait stays unaware of injection.
#[derive(Debug, Clone)]
pub struct XidInjector {
    histories: Arc<Mutex<Vec<Vec<u32>>>>,
}

/// An XID was aimed at a GPU the backend does not have.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct UnknownGpu {
    gpu: usize,
    gpu_count: usize,
}

impl fmt::Display for UnknownGpu {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "gpu {} does not exist, there are {}",
            self.gpu, self.gpu_count
        )
    }
}

impl std::error::Error for UnknownGpu {}

impl XidInjector {
    fn new(gpu_count: usize) -> Self {
        Self {
            histories: Arc::new(Mutex::new(vec![Vec::new(); gpu_count])),
        }
    }

    /// Appends `code` to the GPU's XID history, seen from the next sample on.
    pub fn inject(&self, gpu: usize, code: u32) -> Result<(), UnknownGpu> {
        let mut histories = self.lock();
        let gpu_count = histories.len();
        histories
            .get_mut(gpu)
            .ok_or(UnknownGpu { gpu, gpu_count })?
            .push(code);
        Ok(())
    }

    /// Forgets every injected XID, like a GPU reset.
    pub fn clear(&self) {
        self.lock().iter_mut().for_each(Vec::clear);
    }

    /// Every GPU's history, read under one lock so a sample never sees a
    /// clear applied to only some GPUs.
    fn snapshot(&self) -> Vec<Vec<u32>> {
        self.lock().clone()
    }

    fn lock(&self) -> MutexGuard<'_, Vec<Vec<u32>>> {
        // Inject and clear cannot leave the histories half-updated, so state
        // behind a poisoned lock is still valid.
        self.histories
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
    }
}

/// Clones share one `XidInjector`.
#[derive(Debug, Clone)]
pub struct FakeBackend {
    hostname: String,
    xids: XidInjector,
}

impl FakeBackend {
    pub fn new(gpu_count: usize, hostname: impl Into<String>) -> Self {
        Self {
            hostname: hostname.into(),
            xids: XidInjector::new(gpu_count),
        }
    }

    pub fn injector(&self) -> XidInjector {
        self.xids.clone()
    }

    fn gpu_sample(&self, index: usize, at: UnixSeconds, xid_errors: Vec<u32>) -> GpuSample {
        let shifted = phase_shifted(at, index);
        let temp_wave = triangle_permille(shifted, TEMP_PERIOD_SECS);
        let power_wave = triangle_permille(shifted, POWER_PERIOD_SECS);
        let deciwatts = lerp(POWER_MIN_DECIWATTS, POWER_MAX_DECIWATTS, power_wave);
        GpuSample {
            index,
            uuid: format!("GPU-{}-{index}", self.hostname),
            model: MODEL_NAME.to_owned(),
            temperature_c: lerp(TEMP_MIN_C, TEMP_MAX_C, temp_wave),
            power_w: f64::from(deciwatts) / DECIWATTS_PER_WATT,
            ecc_uncorrected: 0,
            xid_errors,
        }
    }
}

impl GpuBackend for FakeBackend {
    fn sample(&self, at: UnixSeconds) -> Result<Vec<GpuSample>, BackendError> {
        Ok(self
            .xids
            .snapshot()
            .into_iter()
            .enumerate()
            .map(|(index, xids)| self.gpu_sample(index, at, xids))
            .collect())
    }
}

fn phase_shifted(at: UnixSeconds, gpu_index: usize) -> UnixSeconds {
    let offset = u64::try_from(gpu_index)
        .unwrap_or_default()
        .wrapping_mul(GPU_PHASE_STEP_SECS);
    at.wrapping_add(offset)
}

/// Triangle wave over `period_secs` (must be even and non-zero) scaled to
/// `0..=PERMILLE_MAX`. Integer arithmetic keeps it exact on every platform.
fn triangle_permille(at: UnixSeconds, period_secs: u32) -> u32 {
    let half = period_secs / 2;
    // The remainder of a division by a u32 always fits in u32.
    let pos = u32::try_from(at % u64::from(period_secs)).expect("remainder fits u32");
    let rise = if pos < half { pos } else { period_secs - pos };
    rise * PERMILLE_MAX / half
}

fn lerp(min: u32, max: u32, permille: u32) -> u32 {
    min + (max - min) * permille / PERMILLE_MAX
}

#[cfg(test)]
mod tests {
    use super::*;

    const HOST: &str = "node-a";

    fn backend() -> FakeBackend {
        FakeBackend::new(8, HOST)
    }

    fn sample_at(backend: &FakeBackend, at: UnixSeconds) -> Vec<GpuSample> {
        backend.sample(at).expect("fake backend never fails")
    }

    #[test]
    fn reports_one_healthy_sample_per_gpu() {
        let samples = sample_at(&backend(), 1_000);
        assert_eq!(samples.len(), 8);
        assert!(samples.iter().all(GpuSample::is_healthy));
        let indexes: Vec<usize> = samples.iter().map(|s| s.index).collect();
        assert_eq!(indexes, (0..8).collect::<Vec<_>>());
    }

    #[test]
    fn identity_labels_follow_the_contract() {
        let samples = sample_at(&backend(), 0);
        assert_eq!(samples[3].uuid, "GPU-node-a-3");
        assert!(samples.iter().all(|s| s.model == "NVIDIA H100 80GB HBM3"));
    }

    #[test]
    fn same_time_gives_identical_samples() {
        assert_eq!(
            sample_at(&backend(), 123_456),
            sample_at(&backend(), 123_456)
        );
    }

    #[test]
    fn values_at_pinned_times() {
        let b = backend();
        let at = |t: UnixSeconds, gpu: usize| {
            let s = sample_at(&b, t).swap_remove(gpu);
            (s.temperature_c, s.power_w)
        };
        assert_eq!(at(0, 0), (40, 100.0));
        assert_eq!(at(60, 0), (60, 700.0));
        assert_eq!(at(120, 0), (80, 100.0));
        assert_eq!(at(0, 1), (45, 269.8));
    }

    #[test]
    fn values_stay_in_contract_ranges() {
        let b = backend();
        for t in 0..600 {
            for s in sample_at(&b, t) {
                assert!((40..=80).contains(&s.temperature_c), "temp {s:?}");
                assert!((100.0..=700.0).contains(&s.power_w), "power {s:?}");
            }
        }
    }

    #[test]
    fn values_move_over_time_and_differ_between_gpus() {
        let b = backend();
        assert_ne!(
            sample_at(&b, 0)[0].temperature_c,
            sample_at(&b, 60)[0].temperature_c
        );
        let at_zero = sample_at(&b, 0);
        assert!((at_zero[0].power_w - at_zero[1].power_w).abs() > 1.0);
    }

    #[test]
    fn periodic_in_time() {
        let b = backend();
        assert_eq!(sample_at(&b, 5), sample_at(&b, 5 + 240));
    }

    fn inject(backend: &FakeBackend, gpu: usize, code: u32) {
        backend
            .injector()
            .inject(gpu, code)
            .expect("gpu exists in the test backend");
    }

    #[test]
    fn injected_fatal_xid_marks_only_that_gpu_unhealthy() {
        let b = backend();
        inject(&b, 3, 79);
        let samples = sample_at(&b, 42);
        let unhealthy: Vec<usize> = samples
            .iter()
            .filter(|s| !s.is_healthy())
            .map(|s| s.index)
            .collect();
        assert_eq!(unhealthy, vec![3]);
        assert_eq!(samples[3].latest_xid(), 79);
        assert_eq!(samples[2].latest_xid(), 0);
    }

    #[test]
    fn injected_non_fatal_xid_keeps_gpu_healthy() {
        let b = FakeBackend::new(2, HOST);
        inject(&b, 0, 13);
        let samples = sample_at(&b, 42);
        assert!(samples[0].is_healthy());
        assert_eq!(samples[0].latest_xid(), 13);
    }

    #[test]
    fn nothing_is_injected_by_default() {
        let samples = sample_at(&backend(), 42);
        assert!(samples.iter().all(|s| s.xid_errors.is_empty()));
    }

    #[test]
    fn injection_shows_from_the_next_sample_on() {
        let b = backend();
        assert!(sample_at(&b, 1)[2].is_healthy());
        inject(&b, 2, 79);
        let after = sample_at(&b, 2);
        assert!(!after[2].is_healthy());
        assert_eq!(after[2].latest_xid(), 79);
    }

    #[test]
    fn injections_append_to_the_history_in_order() {
        let b = backend();
        inject(&b, 1, 79);
        inject(&b, 1, 13);
        inject(&b, 0, 31);
        let samples = sample_at(&b, 42);
        assert_eq!(samples[1].xid_errors, vec![79, 13]);
        assert_eq!(samples[0].xid_errors, vec![31]);
        assert!(!samples[1].is_healthy(), "fatal XID stays in the history");
        assert_eq!(samples[1].latest_xid(), 13);
    }

    #[test]
    fn clear_restores_every_gpu() {
        let b = backend();
        inject(&b, 1, 79);
        inject(&b, 5, 48);
        b.injector().clear();
        let samples = sample_at(&b, 42);
        assert!(samples.iter().all(GpuSample::is_healthy));
        assert!(samples.iter().all(|s| s.latest_xid() == 0));
    }

    #[test]
    fn injection_after_clear_starts_a_fresh_history() {
        let b = backend();
        inject(&b, 1, 79);
        b.injector().clear();
        inject(&b, 1, 13);
        assert_eq!(sample_at(&b, 42)[1].xid_errors, vec![13]);
    }

    #[test]
    fn unknown_gpu_is_rejected_and_nothing_is_recorded() {
        let b = FakeBackend::new(2, HOST);
        let err = b
            .injector()
            .inject(2, 79)
            .expect_err("gpu 2 is out of range");
        assert_eq!(err.to_string(), "gpu 2 does not exist, there are 2");
        assert!(sample_at(&b, 42).iter().all(GpuSample::is_healthy));
    }

    #[test]
    fn no_gpu_accepts_an_injection_when_there_are_none() {
        let b = FakeBackend::new(0, HOST);
        assert!(b.injector().inject(0, 79).is_err());
    }

    #[test]
    fn backend_clones_share_injected_state() {
        let b = backend();
        let clone = b.clone();
        inject(&b, 4, 79);
        assert_eq!(sample_at(&clone, 42)[4].latest_xid(), 79);
    }

    #[test]
    fn poisoned_lock_does_not_break_injection_or_sampling() {
        let b = backend();
        let injector = b.injector();
        let poisoner = injector.clone();
        let panicked = std::thread::spawn(move || {
            let _guard = poisoner.lock();
            panic!("poison the lock");
        })
        .join();
        assert!(panicked.is_err());
        injector.inject(0, 79).expect("gpu 0 exists");
        assert_eq!(sample_at(&b, 42)[0].latest_xid(), 79);
    }

    #[test]
    fn concurrent_injections_during_sampling_are_all_recorded() {
        const GPUS: usize = 8;
        const PER_GPU: u32 = 200;
        let b = FakeBackend::new(GPUS, HOST);
        let workers: Vec<_> = (0..GPUS)
            .map(|gpu| {
                let injector = b.injector();
                std::thread::spawn(move || {
                    for code in 0..PER_GPU {
                        injector.inject(gpu, code).expect("gpu exists");
                    }
                })
            })
            .collect();

        let expected: Vec<u32> = (0..PER_GPU).collect();
        let mut last_len = [0; GPUS];
        while workers.iter().any(|w| !w.is_finished()) {
            for s in sample_at(&b, 7) {
                let len = s.xid_errors.len();
                assert!(len >= last_len[s.index], "history shrank");
                assert_eq!(s.xid_errors, expected[..len], "gpu {}", s.index);
                last_len[s.index] = len;
            }
        }
        for worker in workers {
            worker.join().expect("injector thread does not panic");
        }

        for s in sample_at(&b, 7) {
            assert_eq!(s.xid_errors, expected, "gpu {}", s.index);
        }
    }

    #[test]
    fn concurrent_injections_on_one_gpu_are_all_recorded() {
        const WORKERS: u32 = 8;
        const PER_WORKER: u32 = 100;
        let b = backend();
        let workers: Vec<_> = (0..WORKERS)
            .map(|w| {
                let injector = b.injector();
                std::thread::spawn(move || {
                    for i in 0..PER_WORKER {
                        injector.inject(0, w * PER_WORKER + i).expect("gpu exists");
                    }
                })
            })
            .collect();
        for _ in 0..50 {
            sample_at(&b, 7);
        }
        for worker in workers {
            worker.join().expect("injector thread does not panic");
        }

        let mut recorded = sample_at(&b, 7).swap_remove(0).xid_errors;
        recorded.sort_unstable();
        assert_eq!(recorded, (0..WORKERS * PER_WORKER).collect::<Vec<_>>());
    }

    #[test]
    fn zero_gpus_yields_no_samples() {
        assert_eq!(sample_at(&FakeBackend::new(0, HOST), 0), Vec::new());
    }
}
