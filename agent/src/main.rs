mod gpu;

use gpu::{FakeBackend, GpuBackend, InjectedXid};

const DEFAULT_FAKE_GPUS: usize = 8;
const FAKE_XID_ENV: &str = "FAKE_XID";

fn main() {
    let mut backend = FakeBackend::new(DEFAULT_FAKE_GPUS);
    if let Ok(raw) = std::env::var(FAKE_XID_ENV) {
        let Some(x) = parse_injected_xid(&raw) else {
            eprintln!("{FAKE_XID_ENV} must look like <gpu>:<code>, got {raw:?}");
            std::process::exit(2);
        };
        backend = backend.with_xid(x);
    }

    for s in backend.sample() {
        println!(
            "gpu={} temp_c={} power_w={:.0} healthy={}",
            s.index,
            s.temperature_c,
            s.power_w,
            s.is_healthy()
        );
    }
}

fn parse_injected_xid(raw: &str) -> Option<InjectedXid> {
    let (gpu, code) = raw.split_once(':')?;
    Some(InjectedXid {
        gpu: gpu.trim().parse().ok()?,
        code: code.trim().parse().ok()?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_gpu_and_code() {
        let x = parse_injected_xid("3:79").expect("valid input");
        assert_eq!((x.gpu, x.code), (3, 79));
    }

    #[test]
    fn rejects_malformed_input() {
        for raw in ["", "3", "a:79", "3:", "3:x"] {
            assert!(parse_injected_xid(raw).is_none(), "{raw:?} should fail");
        }
    }
}
