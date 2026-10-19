//! HTTP surface: `/metrics` encodes the latest published snapshot and
//! `/healthz` reports the process is serving.

use std::sync::Arc;

use axum::Router;
use axum::extract::State;
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::get;

use crate::metrics::{CONTENT_TYPE, Metrics};
use crate::sampler::SnapshotReceiver;

#[derive(Clone)]
pub struct AppState {
    metrics: Arc<Metrics>,
    snapshots: SnapshotReceiver,
}

impl AppState {
    pub fn new(metrics: Arc<Metrics>, snapshots: SnapshotReceiver) -> Self {
        Self { metrics, snapshots }
    }
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/metrics", get(scrape))
        .route("/healthz", get(healthz))
        .with_state(state)
}

async fn scrape(State(state): State<AppState>) -> Response {
    let snapshot = state.snapshots.borrow().clone();
    match state.metrics.render(&snapshot) {
        Ok(body) => ([(header::CONTENT_TYPE, CONTENT_TYPE)], body).into_response(),
        Err(e) => {
            eprintln!("gpu-agent: encoding metrics failed: {e}");
            StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

async fn healthz() -> &'static str {
    "ok\n"
}

#[cfg(test)]
mod tests {
    use axum::body::{Body, to_bytes};
    use axum::http::Request;
    use tokio::sync::watch;
    use tower::ServiceExt;

    use super::*;
    use crate::gpu::{GpuSample, Snapshot, test_sample};

    const BODY_LIMIT: usize = 1 << 20;

    fn app(snapshot: Snapshot) -> Router {
        let (_publisher, receiver) = watch::channel(Arc::new(snapshot));
        router(AppState::new(Arc::new(Metrics::new("node-a")), receiver))
    }

    async fn get_path(app: Router, path: &str) -> Response {
        let request = Request::builder()
            .uri(path)
            .body(Body::empty())
            .expect("valid request");
        app.oneshot(request).await.expect("router is infallible")
    }

    async fn body_text(response: Response) -> String {
        let bytes = to_bytes(response.into_body(), BODY_LIMIT)
            .await
            .expect("body is readable");
        String::from_utf8(bytes.to_vec()).expect("body is utf-8")
    }

    #[tokio::test]
    async fn metrics_serves_openmetrics_text() {
        let snapshot = Snapshot {
            taken_at: 1_760_000_000,
            gpus: vec![GpuSample {
                temperature_c: 62,
                ..test_sample(0)
            }],
        };

        let response = get_path(app(snapshot), "/metrics").await;

        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            response.headers()[header::CONTENT_TYPE],
            "application/openmetrics-text; version=1.0.0; charset=utf-8"
        );
        let body = body_text(response).await;
        assert!(body.contains("DCGM_FI_DEV_GPU_TEMP{"), "{body}");
        assert!(body.contains("} 62\n"), "{body}");
        assert!(body.ends_with("# EOF\n"), "{body}");
    }

    #[tokio::test]
    async fn metrics_before_first_sample_still_serves_self_metrics() {
        let response = get_path(app(Snapshot::default()), "/metrics").await;

        assert_eq!(response.status(), StatusCode::OK);
        let body = body_text(response).await;
        assert!(!body.contains("DCGM_FI_DEV_GPU_TEMP"), "{body}");
        assert!(
            body.contains("gpu_agent_last_sample_timestamp_seconds{Hostname=\"node-a\"} 0\n"),
            "{body}"
        );
    }

    #[tokio::test]
    async fn metrics_reflect_the_latest_published_snapshot() {
        let (publisher, receiver) = watch::channel(Arc::new(Snapshot::default()));
        let router = router(AppState::new(Arc::new(Metrics::new("node-a")), receiver));

        publisher.send_replace(Arc::new(Snapshot {
            taken_at: 42,
            gpus: vec![test_sample(0)],
        }));
        let body = body_text(get_path(router, "/metrics").await).await;

        assert!(body.contains("gpu_agent_last_sample_timestamp_seconds{Hostname=\"node-a\"} 42\n"));
    }

    #[tokio::test]
    async fn healthz_is_ok() {
        let response = get_path(app(Snapshot::default()), "/healthz").await;
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn unknown_path_is_not_found() {
        let response = get_path(app(Snapshot::default()), "/nope").await;
        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }
}
