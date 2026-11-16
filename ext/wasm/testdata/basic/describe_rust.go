//go:build claimrust

package main

// describe, built with -tags claimrust: a manifest that says Rust (the module says Go, which wins), with examples.
const describe = `{"language": "Rust", "functions": ["run", "echo"],
  "repository": "https://github.com/pockethost/pocketone/tree/main/ext/wasm/testdata/basic",
  "examples": [{"input": {"text": "x"}}, {"function": "echo", "input": "hi", "output": "hi"},
    {"function": "echo", "input": {"a":1}, "output": {"a":1}}]}`
