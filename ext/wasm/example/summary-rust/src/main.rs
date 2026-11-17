//! The summary module in Rust: the same input, output and errors as ../summary (Go), a plain program too.
//!
//!   stdin:  {"title": "...", "body": "..."}
//!   stdout: {"text": "<title>: <first 12 words> …", "words": <word count>}
//!
//! Build: cargo build --release --target wasm32-wasip1 (rustup target add wasm32-wasip1 first);
//! the module is target/wasm32-wasip1/release/summary.wasm.
use serde::{Deserialize, Serialize};

#[derive(Deserialize)]
struct Post {
    #[serde(default)]
    title: String,
    #[serde(default)]
    body: String,
}

#[derive(Serialize)]
struct Summary {
    text: String,
    words: usize,
}

/// Ends the program with an error pb_hooks can read (err.code / err.message).
fn fail(code: &str, message: &str) -> ! {
    eprint!("{}", serde_json::json!({ "code": code, "message": message }));
    std::process::exit(1)
}

fn main() {
    // the function asked for: --describe prints the manifest (manifest.json beside the source); "run" is the only other
    match std::env::args().nth(1).as_deref() {
        Some("--describe") => {
            print!("{}", include_str!("../manifest.json"));
            return;
        }
        Some("run") => {}
        other => fail("no_such_function", other.unwrap_or("")),
    }
    let post: Post = serde_json::from_reader(std::io::stdin())
        .unwrap_or_else(|e| fail("bad_input", &format!("the input isn't a post: {e}")));
    if post.body.trim().is_empty() {
        fail("empty_body", "the post has no body to summarize");
    }
    let words: Vec<&str> = post.body.split_whitespace().collect();
    let mut short = words[..words.len().min(12)].join(" ");
    if words.len() > 12 {
        short.push_str(" …");
    }
    let text = if post.title.is_empty() { short } else { format!("{}: {}", post.title, short) };
    serde_json::to_writer(std::io::stdout(), &Summary { text, words: words.len() }).unwrap();
}
