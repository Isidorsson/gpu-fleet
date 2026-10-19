//! Body of `POST /fake/xid`: `{"gpu": N, "code": N}`.

use serde::Deserialize;

#[derive(Debug, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InjectRequest {
    pub gpu: usize,
    pub code: u32,
}

pub fn parse_request(body: &[u8]) -> Result<InjectRequest, String> {
    serde_json::from_slice(body).map_err(|e| e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_either_field_order() {
        let want = InjectRequest { gpu: 3, code: 79 };
        assert_eq!(parse_request(br#"{"gpu": 3, "code": 79}"#), Ok(want));
        assert_eq!(
            parse_request(br#" {"code":79,"gpu":3} "#),
            Ok(InjectRequest { gpu: 3, code: 79 })
        );
    }

    #[test]
    fn rejects_malformed_bodies() {
        for body in [
            &br#"{"gpu": 3}"#[..],
            br#"{"gpu": 3, "code": 79, "x": 1}"#,
            br#"{"gpu": 3, "gpu": 4, "code": 79}"#,
            br#"{"gpu": "3", "code": 79}"#,
            br#"{"gpu": -1, "code": 79}"#,
            br#"{"gpu": 3, "code": 1.5}"#,
            br#"{"gpu": 3, "code": 4294967296}"#,
            br#"{"gpu": 3, "code": 79} trailing"#,
            b"not json",
            b"",
        ] {
            assert!(
                parse_request(body).is_err(),
                "{:?} should be rejected",
                String::from_utf8_lossy(body)
            );
        }
    }
}
