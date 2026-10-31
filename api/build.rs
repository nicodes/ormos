use std::{env, fs, path::Path};
fn walk(root: &Path, dir: &Path, entries: &mut Vec<(String, String)>) {
    for entry in fs::read_dir(dir).expect("UI assets must be built before the API") {
        let entry = entry.unwrap();
        let kind = entry.file_type().unwrap();
        let path = entry.path();
        if kind.is_dir() {
            walk(root, &path, entries);
        } else if kind.is_file() {
            entries.push((
                path.strip_prefix(root)
                    .unwrap()
                    .to_str()
                    .unwrap()
                    .replace('\\', "/"),
                path.to_str().unwrap().into(),
            ));
        } else {
            panic!("UI asset must be a regular file");
        }
    }
}
fn main() {
    let root = Path::new(&env::var("CARGO_MANIFEST_DIR").unwrap())
        .join("../internal/ui/dist")
        .canonicalize()
        .unwrap();
    println!("cargo:rerun-if-changed={}", root.display());
    let mut entries = Vec::new();
    walk(&root, &root, &mut entries);
    entries.sort();
    let mut source = String::from("pub static ASSETS: &[(&str,&[u8])] = &[\n");
    for (name, path) in entries {
        source.push_str(&format!("({name:?},include_bytes!({path:?})),\n"));
    }
    source.push_str("];\n");
    fs::write(
        Path::new(&env::var("OUT_DIR").unwrap()).join("assets.rs"),
        source,
    )
    .unwrap();
}
