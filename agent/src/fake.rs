//! Simulated GPUs for machines without NVIDIA hardware. Readings are a pure
//! function of (gpu index, time) so graphs move and tests can pin time.

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

#[derive(Debug, Clone)]
pub struct FakeBackend {
    gpu_count: usize,
    hostname: String,
    injected: Option<InjectedXid>,
}

impl FakeBackend {
    pub fn new(gpu_count: usize, hostname: impl Into<String>) -> Self {
        Self {
            gpu_count,
            hostname: hostname.into(),
            injected: None,
        }
    }

    #[must_use]
    pub fn with_xid(self, injected: Option<InjectedXid>) -> Self {
        Self { injected, ..self }
    }

    fn gpu_sample(&self, index: usize, at: UnixSeconds) -> GpuSample {
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
            xid_errors: self
                .injected
                .filter(|x| x.gpu == index)
                .map(|x| vec![x.code])
                .unwrap_or_default(),
        }
    }
}

impl GpuBackend for FakeBackend {
    fn sample(&self, at: UnixSeconds) -> Result<Vec<GpuSample>, BackendError> {
        Ok((0..self.gpu_count)
            .map(|index| self.gpu_sample(index, at))
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

    #[test]
    fn injected_fatal_xid_marks_only_that_gpu_unhealthy() {
        let b = backend().with_xid(Some(InjectedXid { gpu: 3, code: 79 }));
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
        let b = FakeBackend::new(2, HOST).with_xid(Some(InjectedXid { gpu: 0, code: 13 }));
        let samples = sample_at(&b, 42);
        assert!(samples[0].is_healthy());
        assert_eq!(samples[0].latest_xid(), 13);
    }

    #[test]
    fn zero_gpus_yields_no_samples() {
        assert!(sample_at(&FakeBackend::new(0, HOST), 0).is_empty());
    }
}
