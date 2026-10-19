//! Runtime configuration, read once from the environment at startup.

use std::env::VarError;
use std::fmt;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::time::Duration;

use crate::fake::InjectedXid;

const LISTEN_VAR: &str = "AGENT_LISTEN";
const SAMPLE_INTERVAL_VAR: &str = "AGENT_SAMPLE_INTERVAL_SECS";
const HOSTNAME_VAR: &str = "AGENT_HOSTNAME";
const SYSTEM_HOSTNAME_VAR: &str = "HOSTNAME";
const FAKE_GPUS_VAR: &str = "FAKE_GPUS";
const FAKE_XID_VAR: &str = "FAKE_XID";

const DEFAULT_LISTEN: SocketAddr = SocketAddr::new(IpAddr::V4(Ipv4Addr::UNSPECIFIED), 9400);
const DEFAULT_SAMPLE_INTERVAL_SECS: u64 = 5;
const DEFAULT_HOSTNAME: &str = "localhost";
const DEFAULT_FAKE_GPUS: usize = 8;
const MAX_FAKE_GPUS: usize = 256;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Config {
    pub listen: SocketAddr,
    pub sample_interval: Duration,
    pub hostname: String,
    pub fake_gpus: usize,
    pub fake_xid: Option<InjectedXid>,
}

impl Config {
    pub fn from_env() -> Result<Self, ConfigError> {
        Self::from_vars(|name| std::env::var(name))
    }

    fn from_vars(get: impl Fn(&str) -> Result<String, VarError>) -> Result<Self, ConfigError> {
        let vars = Vars { get };
        let fake_gpus = vars.parsed(FAKE_GPUS_VAR, DEFAULT_FAKE_GPUS, parse_fake_gpus)?;
        Ok(Self {
            listen: vars.parsed(LISTEN_VAR, DEFAULT_LISTEN, parse_listen)?,
            sample_interval: Duration::from_secs(vars.parsed(
                SAMPLE_INTERVAL_VAR,
                DEFAULT_SAMPLE_INTERVAL_SECS,
                parse_interval_secs,
            )?),
            hostname: vars.hostname()?,
            fake_gpus,
            fake_xid: vars.fake_xid(fake_gpus)?,
        })
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ConfigError {
    var: &'static str,
    value: String,
    problem: String,
}

impl ConfigError {
    fn new(var: &'static str, value: impl Into<String>, problem: impl Into<String>) -> Self {
        Self {
            var,
            value: value.into(),
            problem: problem.into(),
        }
    }
}

impl fmt::Display for ConfigError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "invalid {}={:?}: {}", self.var, self.value, self.problem)
    }
}

impl std::error::Error for ConfigError {}

struct Vars<F> {
    get: F,
}

impl<F: Fn(&str) -> Result<String, VarError>> Vars<F> {
    /// Trimmed value of `var`, with unset and blank both reading as `None`.
    fn optional(&self, var: &'static str) -> Result<Option<String>, ConfigError> {
        match (self.get)(var) {
            Ok(raw) => Ok(Some(raw.trim().to_owned()).filter(|v| !v.is_empty())),
            Err(VarError::NotPresent) => Ok(None),
            Err(VarError::NotUnicode(raw)) => Err(ConfigError::new(
                var,
                raw.to_string_lossy(),
                "not valid unicode",
            )),
        }
    }

    fn parsed<T>(
        &self,
        var: &'static str,
        default: T,
        parse: impl FnOnce(&str) -> Result<T, String>,
    ) -> Result<T, ConfigError> {
        match self.optional(var)? {
            None => Ok(default),
            Some(raw) => parse(&raw).map_err(|problem| ConfigError::new(var, raw, problem)),
        }
    }

    fn hostname(&self) -> Result<String, ConfigError> {
        if let Some(hostname) = self.optional(HOSTNAME_VAR)? {
            return Ok(hostname);
        }
        Ok(self
            .optional(SYSTEM_HOSTNAME_VAR)?
            .unwrap_or_else(|| DEFAULT_HOSTNAME.to_owned()))
    }

    fn fake_xid(&self, fake_gpus: usize) -> Result<Option<InjectedXid>, ConfigError> {
        let Some(raw) = self.optional(FAKE_XID_VAR)? else {
            return Ok(None);
        };
        let xid = parse_injected_xid(&raw)
            .map_err(|problem| ConfigError::new(FAKE_XID_VAR, &raw, problem))?;
        if xid.gpu >= fake_gpus {
            let problem = format!(
                "gpu {} does not exist, {FAKE_GPUS_VAR} is {fake_gpus}",
                xid.gpu
            );
            return Err(ConfigError::new(FAKE_XID_VAR, raw, problem));
        }
        Ok(Some(xid))
    }
}

fn parse_listen(raw: &str) -> Result<SocketAddr, String> {
    raw.parse()
        .map_err(|e| format!("expected <ip>:<port>, e.g. {DEFAULT_LISTEN} ({e})"))
}

fn parse_interval_secs(raw: &str) -> Result<u64, String> {
    match raw.parse::<u64>() {
        Ok(secs) if secs > 0 => Ok(secs),
        Ok(_) => Err("must be at least 1".to_owned()),
        Err(e) => Err(format!("expected a whole number of seconds ({e})")),
    }
}

fn parse_fake_gpus(raw: &str) -> Result<usize, String> {
    match raw.parse::<usize>() {
        Ok(count) if count <= MAX_FAKE_GPUS => Ok(count),
        Ok(_) => Err(format!("must be at most {MAX_FAKE_GPUS}")),
        Err(e) => Err(format!("expected a whole number ({e})")),
    }
}

fn parse_injected_xid(raw: &str) -> Result<InjectedXid, String> {
    const EXPECTED: &str = "expected <gpu>:<code>, e.g. 3:79";
    let (gpu, code) = raw.split_once(':').ok_or(EXPECTED)?;
    Ok(InjectedXid {
        gpu: gpu.trim().parse().map_err(|_| EXPECTED)?,
        code: code.trim().parse().map_err(|_| EXPECTED)?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config_from(pairs: &[(&str, &str)]) -> Result<Config, ConfigError> {
        Config::from_vars(|name| {
            pairs
                .iter()
                .find(|(key, _)| *key == name)
                .map(|(_, value)| (*value).to_owned())
                .ok_or(VarError::NotPresent)
        })
    }

    fn rejected(pairs: &[(&str, &str)]) -> ConfigError {
        config_from(pairs).expect_err("config should be rejected")
    }

    #[test]
    fn defaults_match_the_contract() {
        let config = config_from(&[]).expect("defaults are valid");
        assert_eq!(
            config,
            Config {
                listen: "0.0.0.0:9400".parse().expect("literal"),
                sample_interval: Duration::from_secs(5),
                hostname: "localhost".to_owned(),
                fake_gpus: 8,
                fake_xid: None,
            }
        );
    }

    #[test]
    fn every_variable_can_be_overridden() {
        let config = config_from(&[
            ("AGENT_LISTEN", "127.0.0.1:9500"),
            ("AGENT_SAMPLE_INTERVAL_SECS", "15"),
            ("AGENT_HOSTNAME", "node-b"),
            ("FAKE_GPUS", "4"),
            ("FAKE_XID", "2:79"),
        ])
        .expect("overrides are valid");
        assert_eq!(config.listen, "127.0.0.1:9500".parse().expect("literal"));
        assert_eq!(config.sample_interval, Duration::from_secs(15));
        assert_eq!(config.hostname, "node-b");
        assert_eq!(config.fake_gpus, 4);
        assert_eq!(config.fake_xid, Some(InjectedXid { gpu: 2, code: 79 }));
    }

    #[test]
    fn ipv6_listen_address_is_accepted() {
        let config = config_from(&[("AGENT_LISTEN", "[::]:9400")]).expect("valid");
        assert!(config.listen.is_ipv6());
    }

    #[test]
    fn hostname_falls_back_to_system_hostname() {
        let config = config_from(&[("HOSTNAME", "abc123")]).expect("valid");
        assert_eq!(config.hostname, "abc123");
    }

    #[test]
    fn agent_hostname_wins_over_system_hostname() {
        let config =
            config_from(&[("HOSTNAME", "abc123"), ("AGENT_HOSTNAME", "gpu-01")]).expect("valid");
        assert_eq!(config.hostname, "gpu-01");
    }

    #[test]
    fn blank_values_read_as_unset() {
        let config = config_from(&[
            ("AGENT_LISTEN", ""),
            ("AGENT_SAMPLE_INTERVAL_SECS", "  "),
            ("AGENT_HOSTNAME", ""),
            ("FAKE_GPUS", ""),
            ("FAKE_XID", ""),
        ])
        .expect("blank means default");
        assert_eq!(config, config_from(&[]).expect("defaults"));
    }

    #[test]
    fn zero_gpus_is_allowed() {
        assert_eq!(
            config_from(&[("FAKE_GPUS", "0")]).expect("valid").fake_gpus,
            0
        );
    }

    #[test]
    fn invalid_values_name_the_variable_and_value() {
        let cases = [
            ("AGENT_LISTEN", "nope"),
            ("AGENT_LISTEN", "9400"),
            ("AGENT_LISTEN", "0.0.0.0:99999"),
            ("AGENT_SAMPLE_INTERVAL_SECS", "0"),
            ("AGENT_SAMPLE_INTERVAL_SECS", "-3"),
            ("AGENT_SAMPLE_INTERVAL_SECS", "1.5"),
            ("AGENT_SAMPLE_INTERVAL_SECS", "soon"),
            ("FAKE_GPUS", "many"),
            ("FAKE_GPUS", "-1"),
            ("FAKE_GPUS", "257"),
            ("FAKE_XID", "3"),
            ("FAKE_XID", "a:79"),
            ("FAKE_XID", "3:"),
            ("FAKE_XID", "3:x"),
        ];
        for (var, value) in cases {
            let err = rejected(&[(var, value)]);
            let message = err.to_string();
            assert!(message.contains(var), "{message}");
            assert!(message.contains(&format!("{value:?}")), "{message}");
        }
    }

    #[test]
    fn fake_xid_must_target_an_existing_gpu() {
        let err = rejected(&[("FAKE_GPUS", "4"), ("FAKE_XID", "4:79")]);
        assert_eq!(
            err.to_string(),
            "invalid FAKE_XID=\"4:79\": gpu 4 does not exist, FAKE_GPUS is 4"
        );
    }

    #[test]
    fn fake_xid_tolerates_whitespace() {
        let config = config_from(&[("FAKE_XID", " 3 : 79 ")]).expect("valid");
        assert_eq!(config.fake_xid, Some(InjectedXid { gpu: 3, code: 79 }));
    }

    #[test]
    fn non_unicode_value_is_rejected() {
        let err = Config::from_vars(|name| match name {
            "AGENT_LISTEN" => Err(VarError::NotUnicode("bad".into())),
            _ => Err(VarError::NotPresent),
        })
        .expect_err("not unicode");
        assert_eq!(
            err.to_string(),
            "invalid AGENT_LISTEN=\"bad\": not valid unicode"
        );
    }

    #[test]
    fn error_message_is_readable() {
        let err = rejected(&[("AGENT_SAMPLE_INTERVAL_SECS", "0")]);
        assert_eq!(
            err.to_string(),
            "invalid AGENT_SAMPLE_INTERVAL_SECS=\"0\": must be at least 1"
        );
    }
}
