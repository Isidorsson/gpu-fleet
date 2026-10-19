//! Prometheus exposition of the latest snapshot. A fresh registry is built
//! per scrape from the immutable snapshot, so encoding never touches the
//! sampler; only the two counters live across scrapes.

use std::borrow::Cow;
use std::fmt;
use std::sync::atomic::{AtomicU32, AtomicU64};

use prometheus_client::encoding::text::encode;
use prometheus_client::metrics::counter::Counter;
use prometheus_client::metrics::family::Family;
use prometheus_client::metrics::gauge::Gauge;
use prometheus_client::registry::Registry;

use crate::gpu::{GpuSample, Snapshot, UnixSeconds};

/// Content type of the text `prometheus_client` encodes.
pub const CONTENT_TYPE: &str = "application/openmetrics-text; version=1.0.0; charset=utf-8";

const LABEL_GPU: &str = "gpu";
const LABEL_UUID: &str = "UUID";
const LABEL_MODEL: &str = "modelName";
const LABEL_HOSTNAME: &str = "Hostname";
const LABEL_NODE_ID: &str = "node_id";

type GpuLabels = [(&'static str, String); 5];
type Gauge32 = Gauge<u32, AtomicU32>;
type Gauge64 = Gauge<u64, AtomicU64>;
type FloatGauge = Gauge<f64, AtomicU64>;

/// Sample counters shared between the sampler (writer) and the scrape
/// handler (reader).
#[derive(Debug, Clone, Default)]
pub struct Counters {
    pub samples: Counter,
    pub errors: Counter,
}

#[derive(Debug)]
pub struct Metrics {
    hostname_label: String,
    node_id_label: String,
    counters: Counters,
}

impl Metrics {
    /// `node_id` may be empty; the label is then exported with an empty value.
    pub fn new(hostname: &str, node_id: &str) -> Self {
        Self {
            hostname_label: escape_label_value(hostname),
            node_id_label: escape_label_value(node_id),
            counters: Counters::default(),
        }
    }

    pub fn counters(&self) -> &Counters {
        &self.counters
    }

    pub fn render(&self, snapshot: &Snapshot) -> Result<String, fmt::Error> {
        let mut registry = Registry::default();
        self.register_gpu_metrics(&mut registry, snapshot);
        self.register_agent_metrics(&mut registry, snapshot.taken_at);
        let mut out = String::new();
        encode(&mut out, &registry)?;
        Ok(out)
    }

    fn register_gpu_metrics(&self, registry: &mut Registry, snapshot: &Snapshot) {
        let families = GpuFamilies::default();
        for sample in &snapshot.gpus {
            families.record(&self.gpu_labels(sample), sample);
        }
        families.register(registry);
    }

    fn gpu_labels(&self, sample: &GpuSample) -> GpuLabels {
        [
            (LABEL_GPU, sample.index.to_string()),
            (LABEL_UUID, escape_label_value(&sample.uuid)),
            (LABEL_MODEL, escape_label_value(&sample.model)),
            (LABEL_HOSTNAME, self.hostname_label.clone()),
            (LABEL_NODE_ID, self.node_id_label.clone()),
        ]
    }

    fn register_agent_metrics(&self, registry: &mut Registry, last_sample: UnixSeconds) {
        let labels = [
            (
                Cow::Borrowed(LABEL_HOSTNAME),
                Cow::Owned(self.hostname_label.clone()),
            ),
            (
                Cow::Borrowed(LABEL_NODE_ID),
                Cow::Owned(self.node_id_label.clone()),
            ),
        ];
        let agent = registry.sub_registry_with_labels(labels.into_iter());
        agent.register(
            "gpu_agent_samples",
            "Successful backend samples",
            self.counters.samples.clone(),
        );
        agent.register(
            "gpu_agent_sample_errors",
            "Failed backend samples",
            self.counters.errors.clone(),
        );
        let timestamp = Gauge64::default();
        timestamp.set(last_sample);
        agent.register(
            "gpu_agent_last_sample_timestamp_seconds",
            "Unix time of the last successful sample",
            timestamp,
        );
    }
}

/// `prometheus_client` writes label values verbatim, so quotes, backslashes
/// and newlines must be escaped here or one odd value breaks the whole scrape.
fn escape_label_value(raw: &str) -> String {
    let mut escaped = String::with_capacity(raw.len());
    for c in raw.chars() {
        match c {
            '\\' => escaped.push_str(r"\\"),
            '"' => escaped.push_str(r#"\""#),
            '\n' => escaped.push_str(r"\n"),
            _ => escaped.push(c),
        }
    }
    escaped
}

#[derive(Default)]
struct GpuFamilies {
    temperature: Family<GpuLabels, Gauge32>,
    power: Family<GpuLabels, FloatGauge>,
    xid: Family<GpuLabels, Gauge32>,
    ecc_uncorrected: Family<GpuLabels, Gauge64>,
    healthy: Family<GpuLabels, Gauge32>,
}

impl GpuFamilies {
    fn record(&self, labels: &GpuLabels, sample: &GpuSample) {
        self.temperature
            .get_or_create(labels)
            .set(sample.temperature_c);
        self.power.get_or_create(labels).set(sample.power_w);
        self.xid.get_or_create(labels).set(sample.latest_xid());
        self.ecc_uncorrected
            .get_or_create(labels)
            .set(sample.ecc_uncorrected);
        self.healthy
            .get_or_create(labels)
            .set(u32::from(sample.is_healthy()));
    }

    fn register(self, registry: &mut Registry) {
        registry.register(
            "DCGM_FI_DEV_GPU_TEMP",
            "GPU temperature in degrees Celsius",
            self.temperature,
        );
        registry.register("DCGM_FI_DEV_POWER_USAGE", "Power draw in watts", self.power);
        registry.register(
            "DCGM_FI_DEV_XID_ERRORS",
            "Most recent XID error code, 0 when none",
            self.xid,
        );
        registry.register(
            "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL",
            "Uncorrected (double-bit) ECC errors",
            self.ecc_uncorrected,
        );
        registry.register(
            "gpu_agent_gpu_healthy",
            "1 unless uncorrected ECC errors or a fatal XID were seen",
            self.healthy,
        );
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gpu::test_sample;

    const HOST: &str = "node-a";
    const NODE_ID: &str = "node-009";
    const TAKEN_AT: UnixSeconds = 1_760_000_000;

    fn snapshot(gpus: Vec<GpuSample>) -> Snapshot {
        Snapshot {
            taken_at: TAKEN_AT,
            gpus,
        }
    }

    fn render(snapshot: &Snapshot) -> String {
        render_as(NODE_ID, snapshot)
    }

    fn render_as(node_id: &str, snapshot: &Snapshot) -> String {
        Metrics::new(HOST, node_id)
            .render(snapshot)
            .expect("encoding succeeds")
    }

    fn has_line(body: &str, line: &str) -> bool {
        body.lines().any(|l| l == line)
    }

    fn gpu0_labels() -> &'static str {
        r#"{gpu="0",UUID="GPU-test-0",modelName="Test GPU",Hostname="node-a",node_id="node-009"}"#
    }

    fn healthy_gpu() -> GpuSample {
        GpuSample {
            temperature_c: 62,
            power_w: 412.5,
            ..test_sample(0)
        }
    }

    fn expected_healthy_body(node_id: &str) -> String {
        let labels = format!(
            r#"{{gpu="0",UUID="GPU-test-0",modelName="Test GPU",Hostname="node-a",node_id="{node_id}"}}"#
        );
        let agent = format!(r#"{{Hostname="node-a",node_id="{node_id}"}}"#);
        format!(
            "# HELP DCGM_FI_DEV_GPU_TEMP GPU temperature in degrees Celsius.
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{labels} 62
# HELP DCGM_FI_DEV_POWER_USAGE Power draw in watts.
# TYPE DCGM_FI_DEV_POWER_USAGE gauge
DCGM_FI_DEV_POWER_USAGE{labels} 412.5
# HELP DCGM_FI_DEV_XID_ERRORS Most recent XID error code, 0 when none.
# TYPE DCGM_FI_DEV_XID_ERRORS gauge
DCGM_FI_DEV_XID_ERRORS{labels} 0
# HELP DCGM_FI_DEV_ECC_DBE_VOL_TOTAL Uncorrected (double-bit) ECC errors.
# TYPE DCGM_FI_DEV_ECC_DBE_VOL_TOTAL gauge
DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{labels} 0
# HELP gpu_agent_gpu_healthy 1 unless uncorrected ECC errors or a fatal XID were seen.
# TYPE gpu_agent_gpu_healthy gauge
gpu_agent_gpu_healthy{labels} 1
# HELP gpu_agent_samples Successful backend samples.
# TYPE gpu_agent_samples counter
gpu_agent_samples_total{agent} 0
# HELP gpu_agent_sample_errors Failed backend samples.
# TYPE gpu_agent_sample_errors counter
gpu_agent_sample_errors_total{agent} 0
# HELP gpu_agent_last_sample_timestamp_seconds Unix time of the last successful sample.
# TYPE gpu_agent_last_sample_timestamp_seconds gauge
gpu_agent_last_sample_timestamp_seconds{agent} 1760000000
# EOF
"
        )
    }

    #[test]
    fn renders_a_healthy_gpu_exactly_with_the_node_id() {
        let body = render(&snapshot(vec![healthy_gpu()]));

        assert_eq!(body, expected_healthy_body(NODE_ID));
    }

    #[test]
    fn unset_node_id_renders_an_empty_label_on_every_metric() {
        let body = render_as("", &snapshot(vec![healthy_gpu()]));

        assert_eq!(body, expected_healthy_body(""));
        assert_eq!(body.matches(r#"node_id="""#).count(), 8, "{body}");
    }

    #[test]
    fn fatal_xid_renders_unhealthy_with_the_code() {
        let gpu = GpuSample {
            xid_errors: vec![79],
            ..test_sample(0)
        };

        let body = render(&snapshot(vec![gpu]));

        let labels = gpu0_labels();
        assert!(
            has_line(&body, &format!("DCGM_FI_DEV_XID_ERRORS{labels} 79")),
            "{body}"
        );
        assert!(
            has_line(&body, &format!("gpu_agent_gpu_healthy{labels} 0")),
            "{body}"
        );
    }

    #[test]
    fn non_fatal_xid_renders_the_code_but_stays_healthy() {
        let gpu = GpuSample {
            xid_errors: vec![13],
            ..test_sample(0)
        };

        let body = render(&snapshot(vec![gpu]));

        let labels = gpu0_labels();
        assert!(
            has_line(&body, &format!("DCGM_FI_DEV_XID_ERRORS{labels} 13")),
            "{body}"
        );
        assert!(
            has_line(&body, &format!("gpu_agent_gpu_healthy{labels} 1")),
            "{body}"
        );
    }

    #[test]
    fn uncorrected_ecc_renders_unhealthy() {
        let gpu = GpuSample {
            ecc_uncorrected: 2,
            ..test_sample(0)
        };

        let body = render(&snapshot(vec![gpu]));

        let labels = gpu0_labels();
        assert!(
            has_line(&body, &format!("DCGM_FI_DEV_ECC_DBE_VOL_TOTAL{labels} 2")),
            "{body}"
        );
        assert!(
            has_line(&body, &format!("gpu_agent_gpu_healthy{labels} 0")),
            "{body}"
        );
    }

    #[test]
    fn every_gpu_gets_its_own_series() {
        let body = render(&snapshot(vec![test_sample(0), test_sample(1)]));

        let mut temps: Vec<&str> = body
            .lines()
            .filter(|l| l.starts_with("DCGM_FI_DEV_GPU_TEMP{"))
            .collect();
        temps.sort_unstable();
        assert_eq!(
            temps,
            [
                r#"DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-test-0",modelName="Test GPU",Hostname="node-a",node_id="node-009"} 50"#,
                r#"DCGM_FI_DEV_GPU_TEMP{gpu="1",UUID="GPU-test-1",modelName="Test GPU",Hostname="node-a",node_id="node-009"} 50"#,
            ],
            "{body}"
        );
    }

    #[test]
    fn metric_types_match_the_contract() {
        let body = render(&snapshot(vec![test_sample(0)]));

        for (name, kind) in [
            ("DCGM_FI_DEV_GPU_TEMP", "gauge"),
            ("DCGM_FI_DEV_POWER_USAGE", "gauge"),
            ("DCGM_FI_DEV_XID_ERRORS", "gauge"),
            ("DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "gauge"),
            ("gpu_agent_gpu_healthy", "gauge"),
            ("gpu_agent_samples", "counter"),
            ("gpu_agent_sample_errors", "counter"),
            ("gpu_agent_last_sample_timestamp_seconds", "gauge"),
        ] {
            assert!(
                has_line(&body, &format!("# TYPE {name} {kind}")),
                "{name}
{body}"
            );
        }
    }

    #[test]
    fn before_the_first_sample_only_agent_metrics_are_present() {
        let body = render(&Snapshot::default());

        assert!(!body.contains("DCGM_FI_DEV"), "{body}");
        assert!(!body.contains("gpu_agent_gpu_healthy"), "{body}");
        assert!(has_line(
            &body,
            r#"gpu_agent_last_sample_timestamp_seconds{Hostname="node-a",node_id="node-009"} 0"#
        ));
        assert!(has_line(
            &body,
            r#"gpu_agent_samples_total{Hostname="node-a",node_id="node-009"} 0"#
        ));
    }

    #[test]
    fn counters_are_read_at_scrape_time() {
        let metrics = Metrics::new(HOST, NODE_ID);
        metrics.counters().samples.inc();
        metrics.counters().samples.inc();
        metrics.counters().errors.inc();

        let body = metrics
            .render(&Snapshot::default())
            .expect("encoding succeeds");

        assert!(
            has_line(
                &body,
                r#"gpu_agent_samples_total{Hostname="node-a",node_id="node-009"} 2"#
            ),
            "{body}"
        );
        assert!(
            has_line(
                &body,
                r#"gpu_agent_sample_errors_total{Hostname="node-a",node_id="node-009"} 1"#
            ),
            "{body}"
        );
    }

    #[test]
    fn label_values_are_escaped() {
        let gpu = GpuSample {
            model: "Weird \"GPU\" \\ one\ntwo".to_owned(),
            ..test_sample(0)
        };

        let body = render(&snapshot(vec![gpu]));

        assert!(
            body.contains(r#"modelName="Weird \"GPU\" \\ one\ntwo""#),
            "{body}"
        );
    }

    #[test]
    fn hostname_label_is_escaped_on_every_metric() {
        let metrics = Metrics::new("bad\"host", NODE_ID);

        let body = metrics
            .render(&snapshot(vec![test_sample(0)]))
            .expect("encoding succeeds");

        assert_eq!(body.matches(r#"Hostname="bad\"host""#).count(), 8, "{body}");
    }

    #[test]
    fn node_id_label_is_escaped_on_every_metric() {
        let body = render_as("q\"b\\n\nx", &snapshot(vec![test_sample(0)]));

        assert_eq!(body.matches(r#"node_id="q\"b\\n\nx""#).count(), 8, "{body}");
    }
}
