//! Simulator-only control of the fake backend, on the same listener as
//! `/metrics`: `POST /fake/xid` injects an XID, `DELETE /fake/xid` clears
//! them all. Both take effect at the next sample.

use axum::Router;
use axum::body::Bytes;
use axum::extract::{DefaultBodyLimit, State};
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::routing::post;

use crate::fake::XidInjector;
use crate::request::parse_request;

const XID_PATH: &str = "/fake/xid";
const MAX_BODY_BYTES: usize = 1024;

pub fn router(injector: XidInjector) -> Router {
    Router::new()
        .route(XID_PATH, post(inject_xid).delete(clear_xids))
        .layer(DefaultBodyLimit::max(MAX_BODY_BYTES))
        .with_state(injector)
}

async fn inject_xid(State(injector): State<XidInjector>, body: Bytes) -> Response {
    let injected = parse_request(&body).and_then(|request| {
        injector
            .inject(request.gpu, request.code)
            .map(|()| request)
            .map_err(|e| e.to_string())
    });
    match injected {
        Ok(request) => {
            eprintln!(
                "gpu-agent: injected XID {} on gpu {}",
                request.code, request.gpu
            );
            StatusCode::NO_CONTENT.into_response()
        }
        Err(problem) => (StatusCode::BAD_REQUEST, format!("{problem}\n")).into_response(),
    }
}

async fn clear_xids(State(injector): State<XidInjector>) -> StatusCode {
    injector.clear();
    eprintln!("gpu-agent: cleared injected XIDs");
    StatusCode::NO_CONTENT
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use axum::body::{Body, to_bytes};
    use axum::http::{Method, Request};
    use tower::ServiceExt;

    use super::*;
    use crate::fake::FakeBackend;
    use crate::gpu::{GpuBackend, GpuSample};
    use crate::metrics::{Counters, Metrics};
    use crate::sampler::Sampler;
    use crate::server::{self, AppState};

    const GPUS: usize = 4;
    const BODY_LIMIT: usize = 1 << 20;

    struct Fixture {
        backend: FakeBackend,
        app: Router,
    }

    fn fixture() -> Fixture {
        let backend = FakeBackend::new(GPUS, "node-a");
        let app = router(backend.injector());
        Fixture { backend, app }
    }

    impl Fixture {
        fn samples(&self) -> Vec<GpuSample> {
            self.backend.sample(100).expect("fake backend never fails")
        }

        async fn send(&self, method: Method, body: &str) -> Response {
            let request = Request::builder()
                .method(method)
                .uri(XID_PATH)
                .header("content-type", "application/json")
                .body(Body::from(body.to_owned()))
                .expect("valid request");
            self.app
                .clone()
                .oneshot(request)
                .await
                .expect("router is infallible")
        }

        async fn post(&self, body: &str) -> Response {
            self.send(Method::POST, body).await
        }
    }

    async fn body_text(response: Response) -> String {
        let bytes = to_bytes(response.into_body(), BODY_LIMIT)
            .await
            .expect("body is readable");
        String::from_utf8(bytes.to_vec()).expect("body is utf-8")
    }

    #[tokio::test]
    async fn post_injects_a_fatal_xid_and_returns_no_content() {
        let f = fixture();

        let response = f.post(r#"{"gpu": 2, "code": 79}"#).await;

        assert_eq!(response.status(), StatusCode::NO_CONTENT);
        assert_eq!(body_text(response).await, "");
        let samples = f.samples();
        assert!(!samples[2].is_healthy());
        assert_eq!(samples[2].latest_xid(), 79);
        assert!(
            samples
                .iter()
                .filter(|s| s.index != 2)
                .all(GpuSample::is_healthy)
        );
    }

    #[tokio::test]
    async fn post_of_a_non_fatal_xid_keeps_the_gpu_healthy() {
        let f = fixture();

        let response = f.post(r#"{"gpu": 0, "code": 13}"#).await;

        assert_eq!(response.status(), StatusCode::NO_CONTENT);
        let samples = f.samples();
        assert!(samples[0].is_healthy());
        assert_eq!(samples[0].latest_xid(), 13);
    }

    #[tokio::test]
    async fn posts_append_to_the_history() {
        let f = fixture();

        f.post(r#"{"gpu": 1, "code": 79}"#).await;
        f.post(r#"{"gpu": 1, "code": 13}"#).await;

        assert_eq!(f.samples()[1].xid_errors, vec![79, 13]);
    }

    #[tokio::test]
    async fn delete_clears_every_injected_xid() {
        let f = fixture();
        f.post(r#"{"gpu": 2, "code": 79}"#).await;
        f.post(r#"{"gpu": 3, "code": 48}"#).await;

        let response = f.send(Method::DELETE, "").await;

        assert_eq!(response.status(), StatusCode::NO_CONTENT);
        assert_eq!(body_text(response).await, "");
        let samples = f.samples();
        assert!(samples.iter().all(GpuSample::is_healthy));
        assert!(samples.iter().all(|s| s.latest_xid() == 0));
    }

    #[tokio::test]
    async fn delete_with_nothing_injected_is_still_no_content() {
        let response = fixture().send(Method::DELETE, "").await;
        assert_eq!(response.status(), StatusCode::NO_CONTENT);
    }

    #[tokio::test]
    async fn a_gpu_beyond_the_fake_gpu_count_is_a_bad_request() {
        let f = fixture();

        let response = f.post(r#"{"gpu": 4, "code": 79}"#).await;

        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        assert_eq!(
            body_text(response).await,
            "gpu 4 does not exist, there are 4\n"
        );
        assert!(f.samples().iter().all(GpuSample::is_healthy));
    }

    #[tokio::test]
    async fn bad_bodies_are_bad_requests_and_record_nothing() {
        let f = fixture();
        for body in [
            "",
            "not json",
            r#"{"gpu": 1}"#,
            r#"{"gpu": 1, "code": 79, "extra": true}"#,
            r#"{"gpu": -1, "code": 79}"#,
            r#"{"gpu": "1", "code": 79}"#,
            r#"{"gpu": 1, "code": 4294967296}"#,
        ] {
            let response = f.post(body).await;
            assert_eq!(response.status(), StatusCode::BAD_REQUEST, "{body:?}");
            assert!(!body_text(response).await.is_empty(), "{body:?}");
        }
        assert!(f.samples().iter().all(|s| s.xid_errors.is_empty()));
    }

    #[tokio::test]
    async fn oversized_body_is_refused() {
        let f = fixture();
        let padding = " ".repeat(MAX_BODY_BYTES);

        let response = f
            .post(&format!(r#"{{"gpu": 1, "code": 79}}{padding}"#))
            .await;

        assert_eq!(response.status(), StatusCode::PAYLOAD_TOO_LARGE);
        assert!(f.samples().iter().all(GpuSample::is_healthy));
    }

    #[tokio::test]
    async fn other_methods_are_not_allowed() {
        let f = fixture();
        for method in [Method::GET, Method::PUT] {
            let response = f.send(method.clone(), "").await;
            assert_eq!(
                response.status(),
                StatusCode::METHOD_NOT_ALLOWED,
                "{method}"
            );
        }
    }

    #[tokio::test]
    async fn injection_reaches_the_metrics_on_the_next_sample_and_delete_clears_it() {
        let backend = FakeBackend::new(GPUS, "node-a");
        let injector = backend.injector();
        let metrics = Arc::new(Metrics::new("node-a", "node-009"));
        let (sampler, snapshots) = Sampler::new(backend, || 100, metrics.counters().clone());
        let app =
            server::router(AppState::new(Arc::clone(&metrics), snapshots)).merge(router(injector));
        let gpu2 = r#"{gpu="2",UUID="GPU-node-a-2",modelName="NVIDIA H100 80GB HBM3",Hostname="node-a",node_id="node-009"}"#;
        let scrape = || async {
            let request = Request::builder()
                .uri("/metrics")
                .body(Body::empty())
                .expect("valid request");
            let response = app.clone().oneshot(request).await.expect("infallible");
            body_text(response).await
        };
        let control = |method: Method, body: &'static str| {
            let request = Request::builder()
                .method(method)
                .uri(XID_PATH)
                .body(Body::from(body))
                .expect("valid request");
            let app = app.clone();
            async move { app.oneshot(request).await.expect("infallible").status() }
        };

        sampler.sample_once().await;
        let before = scrape().await;
        assert!(
            before.contains(&format!("gpu_agent_gpu_healthy{gpu2} 1\n")),
            "{before}"
        );

        assert_eq!(
            control(Method::POST, r#"{"gpu": 2, "code": 79}"#).await,
            StatusCode::NO_CONTENT
        );
        let stale = scrape().await;
        assert!(
            stale.contains(&format!("gpu_agent_gpu_healthy{gpu2} 1\n")),
            "the scrape shows the last sample until the next one"
        );

        sampler.sample_once().await;
        let faulted = scrape().await;
        assert!(
            faulted.contains(&format!("gpu_agent_gpu_healthy{gpu2} 0\n")),
            "{faulted}"
        );
        assert!(
            faulted.contains(&format!("DCGM_FI_DEV_XID_ERRORS{gpu2} 79\n")),
            "{faulted}"
        );

        assert_eq!(control(Method::DELETE, "").await, StatusCode::NO_CONTENT);
        sampler.sample_once().await;
        let recovered = scrape().await;
        assert!(
            recovered.contains(&format!("gpu_agent_gpu_healthy{gpu2} 1\n")),
            "{recovered}"
        );
        assert!(
            recovered.contains(&format!("DCGM_FI_DEV_XID_ERRORS{gpu2} 0\n")),
            "{recovered}"
        );
    }

    #[tokio::test]
    async fn concurrent_posts_during_sampling_are_all_recorded() {
        const POSTS: u32 = 64;
        let backend = FakeBackend::new(GPUS, "node-a");
        let app = router(backend.injector());
        let (sampler, snapshots) = Sampler::new(backend, || 100, Counters::default());

        let posts = async {
            let mut statuses = Vec::new();
            for code in 0..POSTS {
                let gpu = usize::try_from(code).expect("small") % GPUS;
                let body = format!(r#"{{"gpu": {gpu}, "code": {code}}}"#);
                let request = Request::builder()
                    .method(Method::POST)
                    .uri(XID_PATH)
                    .body(Body::from(body))
                    .expect("valid request");
                statuses.push(
                    app.clone()
                        .oneshot(request)
                        .await
                        .expect("infallible")
                        .status(),
                );
                tokio::task::yield_now().await;
            }
            statuses
        };
        let sampling = async {
            for _ in 0..POSTS {
                sampler.sample_once().await;
            }
        };
        let (statuses, ()) = tokio::join!(posts, sampling);

        assert!(statuses.iter().all(|s| *s == StatusCode::NO_CONTENT));
        sampler.sample_once().await;
        let snapshot = snapshots.borrow().clone();
        let mut recorded: Vec<u32> = snapshot
            .gpus
            .iter()
            .flat_map(|g| g.xid_errors.iter().copied())
            .collect();
        recorded.sort_unstable();
        assert_eq!(recorded, (0..POSTS).collect::<Vec<_>>());
    }
}
