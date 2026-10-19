//! GPU telemetry types and the backend abstraction. The real backend will
//! wrap NVML; `FakeBackend` (in `fake.rs`) lets the agent run and be tested
//! on machines without NVIDIA GPUs.

use std::fmt;

pub type UnixSeconds = u64;

#[derive(Debug, Clone, PartialEq)]
pub struct GpuSample {
    pub index: usize,
    pub uuid: String,
    pub model: String,
    pub temperature_c: u32,
    pub power_w: f64,
    pub ecc_uncorrected: u64,
    /// XID codes seen on this GPU, oldest first.
    pub xid_errors: Vec<u32>,
}

impl GpuSample {
    pub fn is_healthy(&self) -> bool {
        self.ecc_uncorrected == 0 && !self.xid_errors.iter().copied().any(is_fatal_xid)
    }

    /// Most recent XID code, `0` when none was seen.
    pub fn latest_xid(&self) -> u32 {
        self.xid_errors.last().copied().unwrap_or(0)
    }
}

/// All GPUs of one successful backend read. The default value is the state
/// before the first successful sample.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Snapshot {
    pub taken_at: UnixSeconds,
    pub gpus: Vec<GpuSample>,
}

#[derive(Debug)]
pub struct BackendError(pub String);

impl fmt::Display for BackendError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for BackendError {}

/// A source of GPU readings. `at` is the sampling time, injected so that
/// time-dependent backends stay deterministic in tests.
pub trait GpuBackend: Send + Sync + 'static {
    fn sample(&self, at: UnixSeconds) -> Result<Vec<GpuSample>, BackendError>;
}

/// XID codes that mean the node should leave service, per NVIDIA's XID
/// catalogue: double-bit ECC (48), `NVLink` (74), fell off the bus (79),
/// contained/uncontained ECC (94, 95) and GSP failures (119, 120).
const FATAL_XIDS: [u32; 7] = [48, 74, 79, 94, 95, 119, 120];

pub fn is_fatal_xid(code: u32) -> bool {
    FATAL_XIDS.contains(&code)
}

#[cfg(test)]
pub fn test_sample(index: usize) -> GpuSample {
    GpuSample {
        index,
        uuid: format!("GPU-test-{index}"),
        model: "Test GPU".to_owned(),
        temperature_c: 50,
        power_w: 300.0,
        ecc_uncorrected: 0,
        xid_errors: vec![],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clean_sample_is_healthy() {
        assert!(test_sample(0).is_healthy());
    }

    #[test]
    fn uncorrected_ecc_is_unhealthy() {
        let sample = GpuSample {
            ecc_uncorrected: 1,
            ..test_sample(0)
        };
        assert!(!sample.is_healthy());
    }

    #[test]
    fn fatal_xid_is_unhealthy() {
        let sample = GpuSample {
            xid_errors: vec![79],
            ..test_sample(0)
        };
        assert!(!sample.is_healthy());
    }

    #[test]
    fn non_fatal_xid_keeps_gpu_healthy() {
        // XID 13 is a graphics engine exception, usually an application bug.
        let sample = GpuSample {
            xid_errors: vec![13],
            ..test_sample(0)
        };
        assert!(sample.is_healthy());
    }

    #[test]
    fn fatal_xid_earlier_in_history_stays_unhealthy() {
        let sample = GpuSample {
            xid_errors: vec![79, 13],
            ..test_sample(0)
        };
        assert!(!sample.is_healthy());
        assert_eq!(sample.latest_xid(), 13);
    }

    #[test]
    fn latest_xid_is_zero_without_errors() {
        assert_eq!(test_sample(0).latest_xid(), 0);
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
