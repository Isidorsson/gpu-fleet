//! GPU telemetry sources. The real backend will wrap NVML; `FakeBackend`
//! lets the agent run and be tested on machines without NVIDIA GPUs.

#[derive(Debug, Clone, PartialEq)]
pub struct GpuSample {
    pub index: usize,
    pub temperature_c: u32,
    pub power_w: f64,
    pub ecc_uncorrected: u64,
    pub xid_errors: Vec<u32>,
}

impl GpuSample {
    pub fn is_healthy(&self) -> bool {
        self.ecc_uncorrected == 0 && !self.xid_errors.iter().copied().any(is_fatal_xid)
    }
}

pub trait GpuBackend {
    fn sample(&self) -> Vec<GpuSample>;
}

/// XID codes that mean the node should leave service, per NVIDIA's XID
/// catalogue: double-bit ECC (48), `NVLink` (74), fell off the bus (79),
/// contained/uncontained ECC (94, 95) and GSP failures (119, 120).
const FATAL_XIDS: [u32; 7] = [48, 74, 79, 94, 95, 119, 120];

pub fn is_fatal_xid(code: u32) -> bool {
    FATAL_XIDS.contains(&code)
}

#[derive(Debug, Clone, Copy)]
pub struct InjectedXid {
    pub gpu: usize,
    pub code: u32,
}

#[derive(Debug, Clone)]
pub struct FakeBackend {
    gpu_count: usize,
    injected: Option<InjectedXid>,
}

impl FakeBackend {
    pub fn new(gpu_count: usize) -> Self {
        Self {
            gpu_count,
            injected: None,
        }
    }

    #[must_use]
    pub fn with_xid(self, injected: InjectedXid) -> Self {
        Self {
            injected: Some(injected),
            ..self
        }
    }
}

const FAKE_BASE_TEMP_C: u32 = 45;
const FAKE_BASE_POWER_W: f64 = 300.0;

impl GpuBackend for FakeBackend {
    fn sample(&self) -> Vec<GpuSample> {
        (0..self.gpu_count)
            .map(|index| GpuSample {
                index,
                temperature_c: FAKE_BASE_TEMP_C + u32::try_from(index).unwrap_or(0),
                power_w: FAKE_BASE_POWER_W,
                ecc_uncorrected: 0,
                xid_errors: self
                    .injected
                    .filter(|x| x.gpu == index)
                    .map(|x| vec![x.code])
                    .unwrap_or_default(),
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fake_reports_one_sample_per_gpu() {
        let samples = FakeBackend::new(8).sample();
        assert_eq!(samples.len(), 8);
        assert!(samples.iter().all(GpuSample::is_healthy));
    }

    #[test]
    fn injected_fatal_xid_marks_only_that_gpu_unhealthy() {
        let samples = FakeBackend::new(8)
            .with_xid(InjectedXid { gpu: 3, code: 79 })
            .sample();
        let unhealthy: Vec<usize> = samples
            .iter()
            .filter(|s| !s.is_healthy())
            .map(|s| s.index)
            .collect();
        assert_eq!(unhealthy, vec![3]);
    }

    #[test]
    fn non_fatal_xid_keeps_gpu_healthy() {
        // XID 13 is a graphics engine exception, usually an application bug.
        let samples = FakeBackend::new(2)
            .with_xid(InjectedXid { gpu: 0, code: 13 })
            .sample();
        assert!(samples[0].is_healthy());
    }

    #[test]
    fn uncorrected_ecc_is_unhealthy() {
        let sample = GpuSample {
            index: 0,
            temperature_c: 50,
            power_w: 300.0,
            ecc_uncorrected: 1,
            xid_errors: vec![],
        };
        assert!(!sample.is_healthy());
    }

    #[test]
    fn fatal_xid_classification() {
        for code in FATAL_XIDS {
            assert!(is_fatal_xid(code), "{code} should be fatal");
        }
        assert!(!is_fatal_xid(13));
        assert!(!is_fatal_xid(31));
    }
}
