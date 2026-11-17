//! memhog, an example module in Rust: it asks for more memory than a call may have (MaxCallMemoryMB, 32 MB by default),
//! to show what happens. "run" allocates the MB its input asks for ({"mb": 64}) the ordinary way: over the limit
//! the allocation fails, Rust aborts, and the call fails as `crashed`, saying which limit it hit; the app goes on.
//! "probe" asks 1 MB at a time with try_reserve, which returns an error instead of aborting, and reports how many MB
//! it got before the limit said no.
//!
//! Build: cargo build --release --target wasm32-wasip1
use serde_json::{json, Value};
use std::io::Read;

const MB: usize = 1 << 20;

fn main() {
    // `<name> --describe`: the manifest, kept as manifest.json beside the source
    if std::env::args().nth(1).as_deref() == Some("--describe") {
        print!("{}", include_str!("../manifest.json"));
        return;
    }
    let mut raw = String::new();
    std::io::stdin().read_to_string(&mut raw).ok();
    let input: Value = serde_json::from_str(&raw).unwrap_or(json!({}));
    match std::env::args().nth(1).as_deref() {
        Some("probe") => probe(),
        Some("run") => run(input["mb"].as_u64().unwrap_or(64) as usize),
        other => {
            eprint!("{}", json!({ "code": "no_such_function", "message": other.unwrap_or("") }));
            std::process::exit(1)
        }
    }
}

// run allocates mb MB, 1 MB at a time, writing into each so it's really used, then says it got them all.
fn run(mb: usize) {
    let mut kept: Vec<Vec<u8>> = Vec::new();
    for i in 1..=mb {
        kept.push(vec![1u8; MB]); // over the limit: "memory allocation of 1048576 bytes failed", then abort
        if i % 8 == 0 {
            eprintln!("allocated {i} MB");
        }
    }
    print!("{}", json!({ "allocated": format!("{mb} MB"), "note": "all of it: under the limit" }));
}

// probe asks for 1 MB at a time until the answer is no, without crashing, and reports how far it got.
fn probe() {
    let mut kept: Vec<Vec<u8>> = Vec::new();
    loop {
        let mut chunk: Vec<u8> = Vec::new();
        if chunk.try_reserve_exact(MB).is_err() || kept.try_reserve(1).is_err() {
            break;
        }
        chunk.resize(MB, 1);
        kept.push(chunk);
        if kept.len() >= 4096 {
            break; // 4 GB, wasm32's own end: there was no limit
        }
    }
    print!("{}", json!({
        "got": format!("{} MB", kept.len()),
        "then": "refused: the call's memory is capped (MaxCallMemoryMB); the program itself, its stack and its input take the rest",
    }));
}
