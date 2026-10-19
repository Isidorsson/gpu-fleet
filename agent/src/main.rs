mod config;
mod fake;
mod gpu;
mod metrics;
mod sampler;
mod server;
mod shutdown;

use std::process::ExitCode;
use std::sync::Arc;

use config::Config;
use fake::FakeBackend;
use metrics::Metrics;
use sampler::{Sampler, system_clock};
use server::AppState;
use shutdown::Shutdown;
use tokio::net::TcpListener;

const EXIT_BAD_CONFIG: u8 = 2;

#[tokio::main(flavor = "current_thread")]
async fn main() -> ExitCode {
    let config = match Config::from_env() {
        Ok(config) => config,
        Err(e) => {
            eprintln!("gpu-agent: {e}");
            return ExitCode::from(EXIT_BAD_CONFIG);
        }
    };
    match run(config).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("gpu-agent: {e}");
            ExitCode::FAILURE
        }
    }
}

async fn run(config: Config) -> Result<(), String> {
    let shutdown = Shutdown::install().map_err(|e| format!("installing signal handlers: {e}"))?;
    let listener = TcpListener::bind(config.listen)
        .await
        .map_err(|e| format!("binding {}: {e}", config.listen))?;

    let backend =
        FakeBackend::new(config.fake_gpus, config.hostname.as_str()).with_xid(config.fake_xid);
    let metrics = Arc::new(Metrics::new(config.hostname.as_str()));
    let (sampler, snapshots) = Sampler::new(backend, system_clock, metrics.counters().clone());
    let sampler_task = tokio::spawn(sampler.run(config.sample_interval));

    eprintln!(
        "gpu-agent: serving {} fake GPUs as {:?} on {}",
        config.fake_gpus, config.hostname, config.listen
    );
    let app = server::router(AppState::new(metrics, snapshots));
    let served = axum::serve(listener, app)
        .with_graceful_shutdown(shutdown.requested())
        .await;

    sampler_task.abort();
    served.map_err(|e| format!("serving: {e}"))
}
