---
owners: ["@octavonu"]
version: 0.0.1
title: "WASM"
group: services
summary: "Bring your own WASM: Go, Rust or Zig plugins called from pb_hooks, each a closed box."
---

# WASM

**Bring your own WASM (BYOW).** Your own code, in Go, Rust, Zig or anything that builds for WASI, in pb_hooks, where
`$os.cmd` and `$os.exec` aren't allowed: a plugin is a small program compiled to WebAssembly that your hooks call like
a function. Input in, output out, or an error. It can't reach the database, files, network, environment or other
processes; your hook gives it what it needs and decides what to do with the result.

```js
// pb_hooks/main.pb.js
onRecordCreate((e) => {
  const out = $wasm.run("summary", { title: e.record.get("title"), body: e.record.get("body") })
  e.record.set("summary", out.text)
  e.next()
}, "posts")
```

Use it for what JS hooks do badly or slowly: parsing (EDIFACT, XML, binary formats), crypto, validation, number
crunching, or a library that only exists in Go, Rust or C.

WASM is an extension; the `.pb.wasm` files it runs are plugins.

## Install

WASM isn't built into `pocketone` yet. In your own build, add it to the [standard set](../standard/README.md),
which every app and every pocker instance gets; the set binds `$wasm` into the JS engine
([`ext/jsvm`](../jsvm/README.md)):

```go
set := standard.New().Add(wasm.New(wasm.Config{})) // $wasm in pb_hooks
```

Without the set, register it and bind it yourself:

```go
p := wasm.New(wasm.Config{})

app := pocketbase.New()
extkit.MustRegister(app, p)
jsvm.MustRegister(app, jsvm.Config{OnInit: func(rt js.Runtime) { extkit.BindJS(app, rt, p) }})
app.Start()
```

`$wasm` needs PocketOne's JS engine; with PocketBase's own (`--jsvm goja`) extensions bind nothing.

The module is `github.com/pockethost/pocketone/ext/wasm`. Plugins are `pb_hooks/<name>.pb.wasm`, beside the JS
hooks; nothing else needs setting up.

## Configure

The defaults suit most apps. To change them, pass them to `wasm.New`:

| Field | Default | What it does |
|---|---|---|
| `Dir` | `pb_hooks` beside the data dir | The plugin folder. |
| `MaxCallMemoryMB` | 32 | Memory each call may allocate (its heap, stack and input). Every call has its own, not shared: 8 calls at once may take 8 × 32 MB. A call that needs more crashes alone. |
| `Timeout` | 5s | Time limit for a call's run (compiling comes before it). |
| `MaxConcurrent` | 8 | Calls running at once in the app. More wait, up to `Timeout`, then fail as `busy`. |
| `MaxModulesMB` | 10 | MB of plugin modules running at once: the `.wasm` sizes of the calls running, added up (file sizes, not memory). More wait, as above. It's also the largest module that runs. |
| `MaxPayload` | 4 MB | Largest input or output of a call. |

There's no `pocketone.yaml` section. Under pocker, each instance can override these (see Integrations).

## Use

### Plugins are files

A plugin is one file beside your JS hooks, `pb_hooks/<name>.pb.wasm`, called as `<name>`:

```
pb_hooks/main.pb.js
pb_hooks/summary.pb.wasm
```

It ships with the rest of your code: git, `pocketone deploy`, or SFTP. Like `node script.js`, every call reads the
plugin's file as it is then. A change in `pb_hooks/` also restarts the app, as for a `.pb.js` (under pocker the new
app takes over once it has started). To turn a plugin off, remove or rename it. Hidden files and files that don't end
in `.pb.wasm` are ignored, so upload a new version under a hidden name (`.summary.pb.wasm.part`) and rename it over the
old one: no call reads it half written.

A plugin that can't run (a broken module, a module over `MaxModulesMB`) fails its calls with `not_loaded` and says
why, and `$wasm.plugins()` says it too; the other plugins run as usual.

### The manifest: `--describe`

A plugin says what it is itself: run as `<name> --describe` (the function asked for is `--describe`, with nothing on
stdin), it prints its manifest. Answering is optional, and so is each field; a plugin that exits non-zero or prints
nothing gets the defaults:

```json
{
  "description": "Summarizes a post: its title and first words, and a word count",
  "version": "1.2.0",
  "functions": ["run"],
  "language": "Rust",
  "repository": "https://github.com/you/summary",
  "examples": [
    {
      "function": "run",
      "input": {"title": "Hello", "body": "one two three"},
      "output": {"text": "Hello: one two three", "words": 3}
    }
  ]
}
```

| Field | |
|---|---|
| `description` | what it does |
| `version` | the author's version, as you write it (`1.2.0`, `2026-11-15`): up to 32 letters, digits and `.+_-`; only shown |
| `functions` | the functions hooks may call (default `["run"]`) |
| `language` | what it's written in, shown when the module doesn't say: a Go module always does, a Rust one unless it's stripped (`strip = true`), a Zig one never |
| `repository` | where its source is: an `http(s)` URL (no `javascript:`, no credentials), at most 256 characters |
| `examples` | up to 16 calls to try it with: `function` (default `run`), `input` (a JSON string is the plugin's stdin as it is, anything else goes as JSON, as with `$wasm.run`) and, optionally, the `output` it gives |

The manifest is only shown, by `$wasm.plugins()`, for people and for tests (run each example and compare its
output): calls don't read it, nothing runs the examples, and nothing fetches the repository. A function that isn't
in `functions` still reaches the plugin, which answers it; by convention, with the code `no_such_function`. Function
names can't start with `-`, so `--describe` is never one of them. Try it locally:
`wazero run summary.pb.wasm --describe`, and an example:
`printf '%s' '{"title":"Hello","body":"one two three"}' | wazero run summary.pb.wasm run`.

The examples keep the manifest as `plugin.json` beside the source and build it in (Go's `//go:embed`, Rust's
`include_str!`, Zig's `@embedFile`), so the plugin is still one file. Any other field is refused: a plugin can't ask
for anything, so a manifest that expects more (dependencies, permissions) is shown as an error in
`$wasm.plugins()`, and the plugin runs with the defaults. A manifest is at most 64 KB. `$wasm.plugins()` checks it
with wazero's interpreter (below), within the plugin's limits.

### Calling from pb_hooks

```js
$wasm.run(name, input)            // the plugin's "run"
$wasm.call(name, function, input) // one of its functions (its first argument)
$wasm.plugins()                   // [{name, running, description, version, functions, language, repository, examples,
                                  //   size, error}]
$wasm.limits()                    // {maxCallMemoryMB, timeoutMs, maxConcurrent, maxModulesMB, maxPayload}
$wasm.inspect(name, function, input) // {output, stderr, error, compileMs, runMs}, for trying a plugin out

await $wasm.run.async(name, input) // the same, off the app's JS VM; also $wasm.call.async, $wasm.inspect.async
```

The input is the plugin's stdin: a string or bytes (an `ArrayBuffer`, a `Uint8Array` or another typed array) as they are, nothing for
`undefined` or `null`, anything else as JSON. So `$wasm.run("edifact", e.record.get("raw"))` hands over the raw text,
byte for byte. The output is the plugin's stdout: parsed if it's JSON, a string if not, `null` if empty.

A failed call throws an `Error` named `WasmError`, with `code`, `message`, `retryable` and `plugin`:

```js
try {
  e.record.set("summary", $wasm.run("summary", post).text)
} catch (err) {
  if (err.name !== "WasmError" || err.code !== "empty_body") throw err
}
```

| Code | |
|---|---|
| `not_found` | no `<name>.pb.wasm` in the folder |
| `not_loaded` | in the folder, but it can't run (the message says why: a broken module, too large), or the app isn't serving |
| `no_such_function` | the plugin has no such function (the code plugins give, by convention) |
| `timeout` | over the time limit (retryable) |
| `crashed` | it trapped, panicked or ran out of memory |
| `too_large` | input or output over the limit |
| `busy` | the calls running left no room for this one (retryable); the message gives the numbers |
| `plugin_error` | it exited non-zero without a code of its own |
| anything else | the plugin's own code (below) |

`$wasm.inspect` makes the same call but never throws for it: it returns the output (or `null`), the `error` as
`{code, message, retryable}` (or `null`), what the plugin wrote to stderr (up to 64 KB), and how long compiling and
running took. It's for pages that let a superuser try a plugin out; hooks use `run` and `call`.

The JS engine runs one VM per app, and `run` and `call` hold it while the plugin compiles and runs: the app's
other hooks and requests wait. That's fine for a plugin that answers in milliseconds (Rust, Zig). For a slow one (a
Go plugin compiles for over a second each call) or many calls at once, use `.async` in an async hook, so the app
serves other requests meanwhile:

```js
onRecordCreate(async (e) => {
  const out = await $wasm.run.async("summary", { title: e.record.get("title"), body: e.record.get("body") })
  e.record.set("summary", out.text)
  return e.next()
}, "posts")
```

The time limit covers the run; compiling comes before it (below).

`$wasm` is declared in each app's `pb_data/types.d.ts` ([`wasm.d.ts`](wasm.d.ts)), for code completion and `tsc`.

### Writing a plugin

A plugin is a plain WASI program (`wasip1`), the way you'd write a command-line tool: read stdin, write stdout, exit.
The function asked for is its first argument (`os.Args[1]`, `"run"` for `$wasm.run`, `--describe` for its manifest,
above). To fail, exit non-zero; if
stderr is a JSON object `{"code", "message", "retryable"}`, hooks get it as is, any other text becomes the
`message` of a `plugin_error`. Anything it writes to stderr on success goes to the app's log (Logs in the
dashboard), so stderr is its log.

**Go** ([`example/summary`](example/summary/main.go)):

```go
//go:embed plugin.json
var manifest string // what --describe prints

func main() {
	if os.Args[1] == "--describe" {
		fmt.Print(manifest)
		return
	}
	var post struct{ Title, Body string }
	if err := json.NewDecoder(os.Stdin).Decode(&post); err != nil {
		fmt.Fprint(os.Stderr, `{"code":"bad_input","message":"not a post"}`)
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"text": post.Title, "words": len(strings.Fields(post.Body))})
}
```

```sh
GOOS=wasip1 GOARCH=wasm go build -o pb_hooks/summary.pb.wasm .
```

**Rust** ([`example/summary-rust`](example/summary-rust/src/main.rs)): a binary crate, serde for JSON.

```sh
rustup target add wasm32-wasip1   # once
cargo build --release --target wasm32-wasip1
cp target/wasm32-wasip1/release/summary.wasm pb_hooks/summary.pb.wasm
```

**Zig** ([`example/summary-zig`](example/summary-zig/src/main.zig)): an executable for `wasm32-wasi`. Set
`stack_size = 1 << 20` in `build.zig`: Zig's default stack is 16 MB, and every call would start with it.

Any language that builds a WASI program works the same way (C, TinyGo, AssemblyScript with WASI, ...). Try a
plugin without an app: `echo '{"title":"Hi","body":"one two"}' | wazero run summary.pb.wasm run`.

Two things are refused when the plugin loads, with a message that says so:

- a library instead of a program: Go's `-buildmode=c-shared`, or any module without `_start` ("not a WASI program");
- a module that imports anything outside `wasi_snapshot_preview1`, such as an [Extism](https://extism.org) plugin
  built with its PDK, or host functions this host doesn't have.

### Which language

| Example plugin | Module | Checked when its file loads | Per call | Memory per call: compiled code + the program's own |
|---|---|---|---|---|
| Go | 4.4 MB | 240 ms | 1.7 s | ~20–30 MB + 3.6 MB |
| Rust | 95 KB | 37 ms | 39 ms | ~2–5 MB + 1.1 MB |
| Zig | 55 KB | 39 ms | 23 ms | ~2.5 MB + 1.2 MB |

Every call compiles the plugin from scratch and runs it, and nothing compiled is kept: a plugin takes memory only
while a call runs, however many plugins the apps have: between calls, nothing. The compiled
code is most of it; the program's own memory (its stack and heap, Go's runtime and garbage collector included) stays
under `MaxCallMemoryMB`. Compiling is almost all of a call's time: the summary itself runs in a few milliseconds. Measured on
a laptop (2026-11-14), one call at a time; the compiled code's size is approximate, and a Go plugin's varies with
what it imports (one that imports `net/http` is ~11 MB).

Go calls compiling at once also slow each other down: one took +19 MB, two +38 MB, four +245 MB (2026-11-16). That's
why the calls running at once are limited by their modules' size, `MaxModulesMB`, and not only by their number. An
admin picks the trade-off with that one number:

| `MaxModulesMB` | Go calls (4.4 MB) at once | Rust or Zig calls (~0.1 MB) | Most memory they take, measured |
|---|---|---|---|
| 5 | 1 | 8 (`MaxConcurrent`) | ~25 MB |
| 10 (default) | 2 | 8 | ~45 MB |
| 20 | 4 | 8 | ~250 MB |

Pick by what the plugin does. Go is the easiest to write and fine where a call can take a second or two (a cron job,
an import, an admin action). A plugin on a hot path (every record save, every request) is better in Rust or Zig,
whose calls are 40 times faster and take a seventh of the memory.

## Integrations

### Works with

| Extension | How they fit |
|---|---|
| [Pocker](../pocker/README.md) | One extension value serves every instance (the standard set). Each instance has its own plugins, in its own `pb_hooks/`, its own call slots and limits. Each call reads the instance's plugin file as it is then; nothing runs between calls (below). A plugin change reloads just that instance, like a `.pb.js` change. |
| [Sandbox](../sandbox/README.md) | `$wasm` is safe to give a hostile tenant: a plugin gets nothing but its input. A call can't be interrupted by the JS engine's time limits, so `Timeout` (and compiling) bound it instead. |
| [Deploy](../deploy/README.md) | `pocketone deploy` sends the plugins with the rest of `pb_hooks/`. |

### Per-instance settings (pocker)

Each instance can override `maxCallMemoryMB`, `maxConcurrent`, `maxModulesMB`, `timeout` (a duration, like `5s`) and
`maxPayload` in its `extensions.wasm` settings, over the Config given to `New`, key by key. `enabled: false` turns
`$wasm` off for that instance (`typeof $wasm === "undefined"` in its hooks). A change restarts just that instance, on
its next request:

```yaml
instances:
  - id: abc
    extensions: { wasm: { timeout: 10s, maxCallMemoryMB: 64 } }
  - id: no-plugins
    extensions: { wasm: { enabled: false } }
```

Leave `dir` unset there: each instance then gets the `pb_hooks/` in its own folder. `MaxConcurrent` and
`MaxModulesMB` count per instance, so one instance's busy plugins never make another's calls wait. A restart (new
settings, a pb_hooks change, a restore) lets calls in progress finish, and the instance's plugins run as before
after it, from its folder: nothing to reload.

### Building on WASM

There's no Go API, endpoint or form for calling a plugin: pb_hooks (`$wasm`) are the only way in, so whatever
reaches a plugin went through the app's own code.

## Example

[`example/main.go`](example/main.go) runs one app with a `posts` collection and the hook above; its folder is laid
out like an instance (`pb_data/`, `pb_hooks/`). Build the plugin into `example/pb_hooks/`, then
`cd example && go run . serve --dev` and create a post.

```go
// Command example runs one app with ext/wasm and the summary plugin wired in from pb_hooks: a new post gets its
// summary before it's saved (pb_hooks/main.pb.js). The example folder is the app's folder, laid out like an
// instance on PocketHost: pb_data/ and pb_hooks/ side by side, the plugin beside the JS hooks.
//
//	cd example && go run . serve --dev
//
// Build the plugin into pb_hooks/ (before or after starting the app: each call reads it):
//
//	(cd summary && GOOS=wasip1 GOARCH=wasm go build -o ../pb_hooks/summary.pb.wasm .)
//
// Then create a post (dashboard, or the API) and look at its summary field.
//
// Logins (ext/devusers, local testing only): dashboard admin@example.com / password, app user user@example.com / password.
package main

import (
	"log"

	"ganigeorgiev.com/pocketbase/pocketbase"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/devusers"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/extkit/js"
	"github.com/pockethost/pocketone/ext/jsvm"
	"github.com/pockethost/pocketone/ext/wasm"
)

func main() {
	app := pocketbase.New()
	devusers.Register(app) // local testing: superuser admin@example.com and user user@example.com, password "password"
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return posts(e.App)
	})
	// the same extension value the standard set gives every pocker instance (set.Add(wasm.New(...)))
	plugins := wasm.New(wasm.Config{})
	extkit.MustRegister(app, plugins)
	jsvm.MustRegister(app, jsvm.Config{
		HooksDir: "pb_hooks",
		OnInit:   func(rt js.Runtime) { extkit.BindJS(app, rt, plugins) }, // $wasm in pb_hooks
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func posts(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("posts"); err == nil {
		return nil
	}
	c := core.NewBaseCollection("posts")
	c.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "body", Max: 100000},
		&core.TextField{Name: "summary", Max: 1000}, &core.NumberField{Name: "words"})
	return app.Save(c)
}
```

### More example plugins

Each is a plain program with a `plugin.json` built in, which it prints for `--describe` (its functions and examples,
so `/wasm`-style pages and the wazero CLI can try it), built with the command in its header. They answer the usual questions about a feature like this:

| Plugin | Language | What it shows |
|---|---|---|
| [`summary`](example/summary), [`summary-rust`](example/summary-rust), [`summary-zig`](example/summary-zig) | Go, Rust, Zig | The same plugin in three languages: the hook above, and how much the language matters (a Go module is ~4 MB and compiles for over a second; Rust and Zig, under 100 KB and ~40 ms). |
| [`crypto`](example/crypto) | Go | What an app used to run with `$os.exec`: AES-256-GCM. The key comes in with each call; the plugin can't keep it or send it anywhere. |
| [`waflows`](example/waflows) | Rust | What pb_hooks can't do: a WhatsApp Flows endpoint needs RSA-OAEP and AES-GCM with WhatsApp's key and IV. The hook keeps the JSON and HTTP; the plugin does only those two. |
| [`edifact-orders-rust`](example/edifact-orders-rust) | Rust | A real format with a real library (edifact-rs): an EDIFACT ORDERS message, as raw text, read into an order. |
| [`order-xml`](example/order-xml) | Rust | The same order from XML (roxmltree), so a hook takes either format and stores one shape. |
| [`devil`](example/devil) | Rust | Is it safe? It tries, in plain sight, to read the app's database and `/etc/passwd`, list folders, write a file, read the environment, run `sh -c id` and send to `attacker.example`. Every try says `refused`. |
| [`memhog`](example/memhog) | Rust | What does it cost? It asks for more memory than `MaxCallMemoryMB`: that call fails as `crashed`, saying which limit it hit, and the app goes on. `probe` shows the limit from inside. |

The Rust and Zig ones are also kept built, beside their sources (`example/<folder>/<name>.pb.wasm`, under 300 KB
each), so they double as the extension's tests: every run of `go test` loads each one, checks that its `--describe`
is its `plugin.json`, and runs its examples, with no Rust or Zig installed. A change to `ext/wasm` that breaks a real
plugin shows there. With Rust 1.100.0 (and `wasm32-wasip1`) or Zig 0.14.0 installed, `TestPrebuiltMatchSources`
also rebuilds each one and checks it's the same bytes; the Rust builds remap their paths (to `/rustc/<commit>` and
`/cargo`), so a module holds no path of the machine that built it. After changing one:
`go test -run TestPrebuiltExamples -update`.

## What a plugin can't do

Plugins run in [wazero](https://wazero.io) (pure Go, no cgo), in the app's process, with only the standard WASI
functions and nothing behind them:

- **No files:** nothing is mounted, not even `pb_hooks/`. `open` fails.
- **No network:** WASI preview 1 has no way to connect. (Go's `net` on wasip1 is an in-memory stand-in: a plugin can
  "listen", but nothing outside the program reaches it.)
- **No environment, no processes**, no arguments beyond the function name.
- **No state between calls:** every call gets a fresh instance, so nothing from one call (or one caller) reaches the
  next.
- **Bounded:** memory, time, input and output size, and how many calls run at once (see Configure).

It does get a real clock and real random numbers (crypto nonces need them). Data from the app, secrets and API
responses reach a plugin only as input from your hook, and the result goes nowhere but back to the hook: the network
stays with `$http.send` in pb_hooks.

## How it works

- Plugins run only while the app serves (other commands, such as `migrate`, never read them).
- Like `node script.js`: each call reads its plugin's file (`<name>.pb.wasm`, once the call has room), compiles the module in a wazero runtime of its own, runs it, and closes it after. Nothing stays in memory
  between calls, so a folder of 100 plugins costs nothing while they aren't called, and no compilation cache, in
  memory or on disk. Compiling validates the module: a broken or half-written one doesn't run. A call in progress
  finishes with what it read, whatever happens to the file meanwhile. Compiling counts toward `MaxConcurrent` and
  `MaxModulesMB`, so the memory it takes stays bounded too.
- WASM watches nothing itself: no watches of its own, no timer, no work between calls, however many instances there
  are. (The JS engine's, or pocker's, watch on `pb_hooks/` restarts the app when a plugin changes, as for a hook.)
- `$wasm.plugins()` lists the folder each time. It checks each module with wazero's interpreter, which validates it
  and runs `--describe` without making machine code (~0.25 s for a Go plugin), and keeps only what it found
  (manifest, language, error) until the plugin's file changes size or time; when the app starts serving, it checks
  them all once, so the log says which ones can't run.
- A call waits for room: fewer than `MaxConcurrent` calls running, and its module's size left in `MaxModulesMB`
  (`$wasm.plugins()` gives each one's `size`). Compiling takes time and memory in step with a module's size, so with
  the defaults (8 calls, 10 MB) a Rust or Zig plugin (~0.1 MB) runs 8 calls at once and a Go one (~4.4 MB) 2, beside
  small ones. A call goes in as soon as there's room for it, so a small call doesn't queue behind a big one that's
  waiting; under a steady stream of small calls, a big one may wait until it gives up as `busy`.
- No collection and no database use: what's in the folder, and why a plugin can't run, comes from the files
  (`$wasm.plugins()` and the log).

## License

MIT
