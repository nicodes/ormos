fn main() {
    println!("cargo:rustc-check-cfg=cfg(bundled_api)");
    for name in ["VERSION", "COMMIT", "ORMOS_API_PAYLOAD", "ORMOS_API_DIGEST"] {
        println!("cargo:rerun-if-env-changed={name}");
    }
    let version = std::env::var("VERSION").unwrap_or_else(|_| "dev".into());
    let version = version.strip_prefix('v').unwrap_or(&version);
    let commit = std::env::var("COMMIT").unwrap_or_else(|_| "unknown".into());
    assert!(
        !version.contains(['\r', '\n']) && !commit.contains(['\r', '\n']),
        "invalid build metadata"
    );
    println!("cargo:rustc-env=ORMOS_VERSION={version}");
    println!("cargo:rustc-env=ORMOS_COMMIT={commit}");
    if let Ok(path) = std::env::var("ORMOS_API_PAYLOAD") {
        assert!(
            std::path::Path::new(&path).is_absolute(),
            "API payload must be absolute"
        );
        let digest = std::env::var("ORMOS_API_DIGEST").expect("API source receipt digest");
        assert!(
            digest.len() == 64
                && digest
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)),
            "invalid API digest"
        );
        println!("cargo:rerun-if-changed={path}");
        println!("cargo:rustc-env=ORMOS_API_PAYLOAD={path}");
        println!("cargo:rustc-env=ORMOS_API_DIGEST={digest}");
        println!("cargo:rustc-cfg=bundled_api");
    } else {
        println!("cargo:rustc-env=ORMOS_API_DIGEST=");
    }
}
