package wasm_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/apis"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tests"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/types"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/extkit/js"
	"github.com/pockethost/pocketone/ext/jsvm"
	"github.com/pockethost/pocketone/ext/sandbox"
	"github.com/pockethost/pocketone/ext/wasm"
)

// hooks is what a programmer writes in pb_hooks: the plugin gets what the hook gives it, the hook decides what to
// save. Errors are caught (results) or not (strict: the save fails).
const hooks = `
onRecordCreate((e) => {
  const out = $wasm.run("basic", { text: e.record.get("title") })
  e.record.set("summary", out.text)
  e.record.set("words", out.words)
  e.next()
}, "posts")

onRecordCreate((e) => {
  try {
    const out = $wasm.call(e.record.get("plugin"), e.record.get("fn"), { text: "x" })
    e.record.set("result", JSON.stringify(out))
  } catch (err) {
    e.record.set("name", err.name)
    e.record.set("code", err.code)
    e.record.set("message", err.message)
    e.record.set("retryable", err.retryable)
    e.record.set("plugin", err.plugin)
  }
  e.record.set("plugins", JSON.stringify($wasm.plugins().filter((p) => p.running).map((p) => p.name)))
  e.next()
}, "results")

onRecordCreate((e) => {
  $wasm.run(e.record.get("plugin"), {})
  e.next()
}, "strict")

// .async: the plugin runs off the app's JS VM, the hook awaits it
onRecordCreate(async (e) => {
  try {
    const out = await $wasm.call.async(e.record.get("plugin"), e.record.get("fn"), { text: e.record.get("title") })
    e.record.set("result", JSON.stringify(out))
  } catch (err) {
    e.record.set("result", err.name + " " + err.code)
  }
  const r = await $wasm.inspect.async("basic", "logs", "")
  e.record.set("stderr", r.stderr)
  return e.next()
}, "awaits")

onRecordCreate((e) => {
  const inputs = {
    string: "UNB+UNOA:2'",
    bytes: new Uint8Array([85, 78, 66]),
    buffer: new Uint8Array([85, 78, 72]).buffer,
    view: new Uint8Array([0, 85, 78, 84, 0]).subarray(1, 4),
    none: null,
    object: { a: 1 },
  }
  const out = $wasm.call("basic", "echo", inputs[e.record.get("kind")])
  e.record.set("result", typeof out + " " + JSON.stringify(out))
  e.next()
}, "echoes")
`

// jsApp is an app with ext/wasm, the basic plugin, and the hooks above; sandboxed runs them under ext/sandbox.
func jsApp(t *testing.T, sandboxed bool) env {
	t.Helper()
	code := module(t, basic)
	return newApp(t, wasm.Config{Timeout: time.Second}, func(app core.App, pluginDir string) {
		posts := core.NewBaseCollection("posts")
		posts.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "summary"}, &core.NumberField{Name: "words"})
		results := core.NewBaseCollection("results")
		for _, f := range []string{"plugin", "fn", "result", "name", "code", "message", "plugins"} {
			results.Fields.Add(&core.TextField{Name: f, Max: 5000})
		}
		results.Fields.Add(&core.BoolField{Name: "retryable"})
		strict := core.NewBaseCollection("strict")
		strict.Fields.Add(&core.TextField{Name: "plugin"})
		echoes := core.NewBaseCollection("echoes")
		echoes.Fields.Add(&core.TextField{Name: "kind"}, &core.TextField{Name: "result"})
		awaits := core.NewBaseCollection("awaits")
		for _, f := range []string{"plugin", "fn", "title", "result", "stderr"} {
			awaits.Fields.Add(&core.TextField{Name: f})
		}
		for _, c := range []*core.Collection{posts, results, strict, echoes, awaits} {
			c.CreateRule = types.Pointer("")
			if err := app.Save(c); err != nil {
				t.Fatal(err)
			}
		}
		install(t, pluginDir, "basic", code)
		install(t, pluginDir, "broken", code[:1000]) // in the folder, but doesn't load

		dir := t.TempDir()
		hooksDir := filepath.Join(dir, "pb_hooks")
		os.MkdirAll(hooksDir, 0o755)
		if err := os.WriteFile(filepath.Join(hooksDir, "main.pb.js"), []byte(hooks), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := jsvm.Config{
			HooksDir: hooksDir,
			OnInit:   func(rt js.Runtime) { wasm.BindJS(app, rt) },
		}
		if sandboxed {
			cfg.Extensions = []any{&sandbox.Sandbox{Dir: dir, MaxExecution: 5 * time.Second}}
		}
		if err := jsvm.Register(app, cfg); err != nil {
			t.Fatal(err)
		}
	})
}

func create(t *testing.T, app core.App, coll string, data map[string]any) (*core.Record, error) {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(coll)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Load(data)
	return r, app.Save(r)
}

// TestJSHooks: $wasm from real pb_hooks, plain and under the sandbox: a computed field set before the save,
// $wasm.call and $wasm.plugins(), the .async twins in an async hook, every error as a WasmError exception with its code, how inputs (string, bytes,
// buffer, null, object) and outputs (JSON, text, empty) cross over, and an uncaught error failing the save.
func TestJSHooks(t *testing.T) {
	for _, sandboxed := range []bool{false, true} {
		name := "plain"
		if sandboxed {
			name = "sandboxed"
		}
		t.Run(name, func(t *testing.T) {
			e := jsApp(t, sandboxed)

			// a computed field, before the save
			p, err := create(t, e.app, "posts", map[string]any{"title": "hello wasm world"})
			if err != nil {
				t.Fatal(err)
			}
			p, _ = e.app.FindRecordById("posts", p.Id)
			if p.GetString("summary") != "HELLO WASM WORLD" || p.GetInt("words") != 3 {
				t.Fatalf("summary %q, words %d", p.GetString("summary"), p.GetInt("words"))
			}

			result := func(plugin, fn string) *core.Record {
				t.Helper()
				r, err := create(t, e.app, "results", map[string]any{"plugin": plugin, "fn": fn})
				if err != nil {
					t.Fatal(err)
				}
				r, _ = e.app.FindRecordById("results", r.Id)
				return r
			}
			wantErr := func(r *core.Record, code, message string, retryable bool) {
				t.Helper()
				if r.GetString("name") != "WasmError" || r.GetString("code") != code ||
					!strings.Contains(r.GetString("message"), message) || r.GetBool("retryable") != retryable {
					t.Fatalf("got name %q code %q message %q retryable %v, want %s %q %v", r.GetString("name"),
						r.GetString("code"), r.GetString("message"), r.GetBool("retryable"), code, message, retryable)
				}
			}

			// $wasm.call: another declared function, output parsed
			r := result("basic", "counter")
			if r.GetString("result") != `{"count":1}` || r.GetString("code") != "" {
				t.Fatalf("counter: %q %q", r.GetString("result"), r.GetString("code"))
			}
			if r.GetString("plugins") != `["basic"]` {
				t.Fatalf("plugins: %s", r.GetString("plugins"))
			}

			// errors, as JS exceptions
			wantErr(result("basic", "fail"), "rate_limited", "try again later", true)
			wantErr(result("basic", "failPlain"), wasm.CodePluginError, "something broke", false)
			wantErr(result("basic", "nosuch"), wasm.CodeNoSuchFunction, "nosuch", false)
			wantErr(result("basic", "loop"), wasm.CodeTimeout, "time limit", true)
			wantErr(result("broken", "run"), wasm.CodeNotLoaded, "didn't load", false)
			wantErr(result("nope", "run"), wasm.CodeNotFound, "no such plugin", false)
			if p := result("nope", "run").GetString("plugin"); p != "nope" {
				t.Fatalf("plugin: %q", p)
			}

			// the input as stdin: strings and bytes as they are, null as nothing, the rest as JSON; the output
			// parsed if it's JSON, a string if not, null if empty
			for kind, want := range map[string]string{
				"string": `string "UNB+UNOA:2'"`,
				"bytes":  `string "UNB"`,
				"buffer": `string "UNH"`,
				"view":   `string "UNT"`,
				"none":   `object null`,
				"object": `object {"a":1}`,
			} {
				r, err := create(t, e.app, "echoes", map[string]any{"kind": kind})
				if err != nil {
					t.Fatalf("%s: %v", kind, err)
				}
				r, _ = e.app.FindRecordById("echoes", r.Id)
				if got := r.GetString("result"); got != want {
					t.Errorf("%s: got %s, want %s", kind, got, want)
				}
			}

			// .async: the same results, awaited
			for _, tc := range []struct{ plugin, fn, want string }{
				{"basic", "run", `{"text":"ASYNC HI","words":2}`},
				{"basic", "fail", "WasmError rate_limited"},
				{"nope", "run", "WasmError not_found"},
			} {
				r, err := create(t, e.app, "awaits", map[string]any{"plugin": tc.plugin, "fn": tc.fn, "title": "async hi"})
				if err != nil {
					t.Fatalf("%s.%s: %v", tc.plugin, tc.fn, err)
				}
				r, _ = e.app.FindRecordById("awaits", r.Id)
				if r.GetString("result") != tc.want || r.GetString("stderr") != "working on it\n" {
					t.Errorf("%s.%s async: %q %q, want %s", tc.plugin, tc.fn, r.GetString("result"), r.GetString("stderr"), tc.want)
				}
			}

			// not caught: the save fails with the plugin's message
			if _, err := create(t, e.app, "strict", map[string]any{"plugin": "nope"}); err == nil ||
				!strings.Contains(err.Error(), "no such plugin") {
				t.Fatalf("strict: %v", err)
			}
			if n, _ := e.app.CountRecords("strict"); n != 0 {
				t.Fatalf("%d strict records saved", n)
			}
		})
	}
}

// TestLoopInHook: a hook that calls a plugin in an endless loop is stopped by the engine's wall-time limit per task
// (TaskWall, Go calls included; pocker sets it from the sandbox's limit), and the plugin keeps working for everyone
// else.
func TestLoopInHook(t *testing.T) {
	// ext/jsvm checks its time limits in QuickJS's interrupt handler, which runs only every several thousand JS
	// operations: a loop over a Go call that takes milliseconds reaches it after minutes, not after TaskWall (a loop
	// over a 20 ms Go function ran 20 s and more with TaskWall 1 s). An engine matter, for Ben; until then this
	// can't pass.
	t.Skip("ext/jsvm: TaskWall doesn't stop a loop over slow Go calls (interrupt handler runs every ~10k ops)")
	code := module(t, basic)
	e := newApp(t, wasm.Config{}, func(app core.App, pluginDir string) {
		c := core.NewBaseCollection("loops")
		c.Fields.Add(&core.TextField{Name: "title"})
		if err := app.Save(c); err != nil {
			t.Fatal(err)
		}
		install(t, pluginDir, "basic", code)
		dir := t.TempDir()
		hooksDir := filepath.Join(dir, "pb_hooks")
		os.MkdirAll(hooksDir, 0o755)
		os.WriteFile(filepath.Join(hooksDir, "loop.pb.js"), []byte(`
onRecordCreate((e) => {
  while (true) { $wasm.run("basic", { text: "x" }) }
}, "loops")`), 0o644)
		if err := jsvm.Register(app, jsvm.Config{
			HooksDir:   hooksDir,
			OnInit:     func(rt js.Runtime) { wasm.BindJS(app, rt) },
			Extensions: []any{&sandbox.Sandbox{Dir: dir, MaxExecution: time.Second}},
			VM:         jsvm.VMConfig{TaskWall: time.Second}, // as pocker sets the sandbox's limit
		}); err != nil {
			t.Fatal(err)
		}
	})
	start := time.Now()
	_, err := create(t, e.app, "loops", map[string]any{"title": "x"})
	if err == nil {
		t.Fatalf("save: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("stopped after %s", d)
	}
	if !runs(t, e, "basic") {
		t.Fatal("the plugin stopped working")
	}
}

// TestBindBeforeRegister: when the JS engine (and so BindJS) starts before the extension is registered, $wasm is
// still there and works.
func TestBindBeforeRegister(t *testing.T) {
	code := module(t, basic)
	app, err := tests.NewTestApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.pb.js"), []byte(`
onRecordCreate((e) => { e.record.set("summary", $wasm.run("basic", { text: e.record.get("title") }).text); e.next() }, "posts")`), 0o644)
	pluginDir := t.TempDir()
	p := wasm.New(wasm.Config{Dir: pluginDir})
	if err := jsvm.Register(app, jsvm.Config{
		HooksDir: dir,
		OnInit:   func(rt js.Runtime) { extkit.BindJS(app, rt, p) }, // the engine starts before MustRegister
	}); err != nil {
		t.Fatal(err)
	}
	extkit.MustRegister(app, p)

	c := core.NewBaseCollection("posts")
	c.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "summary"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	install(t, pluginDir, "basic", code)
	r, _ := apis.NewRouter(app)
	se := &core.ServeEvent{App: app, Router: r, Server: &http.Server{}}
	if err := app.OnServe().Trigger(se, func(e *core.ServeEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.OnTerminate().Trigger(&core.TerminateEvent{App: app}) })

	post, err := create(t, app, "posts", map[string]any{"title": "late binding"})
	if err != nil {
		t.Fatal(err)
	}
	post, _ = app.FindRecordById("posts", post.Id)
	if post.GetString("summary") != "LATE BINDING" {
		t.Fatalf("summary %q", post.GetString("summary"))
	}
}

// TestInspect: $wasm.inspect never throws for the call: it returns the output, what the plugin wrote to stderr, the
// error as plain fields, and how long compiling and running took.
func TestInspect(t *testing.T) {
	e := withBasic(t, wasm.Config{})
	text, err := jsEval(t, e.app, `JSON.stringify([
		$wasm.inspect("basic", "logs", ""),
		$wasm.inspect("basic", "fail", ""),
		$wasm.inspect("basic", "echo", { a: 1 }),
		$wasm.inspect("nope", "run", {}),
	])`)
	if err != nil {
		t.Fatal(err)
	}
	type wasmErr struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}
	var got []struct {
		Output    any      `json:"output"`
		Stderr    string   `json:"stderr"`
		Error     *wasmErr `json:"error"`
		CompileMs float64  `json:"compileMs"`
		RunMs     float64  `json:"runMs"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if r := got[0]; r.Output != "done" || r.Stderr != "working on it\n" || r.Error != nil || r.RunMs <= 0 {
		t.Errorf("logs: %+v", r)
	}
	if r := got[1]; r.Output != nil || r.Error == nil || r.Error.Code != "rate_limited" || !r.Error.Retryable ||
		!strings.Contains(r.Stderr, "rate_limited") {
		t.Errorf("fail: %+v", r)
	}
	if r := got[2]; fmt.Sprint(r.Output) != "map[a:1]" || r.Error != nil {
		t.Errorf("echo: %+v", r)
	}
	if r := got[3]; r.Error == nil || r.Error.Code != wasm.CodeNotFound || r.CompileMs != 0 {
		t.Errorf("not found: %+v", r)
	}
}

// jsEval runs src on a JS VM with $wasm bound to app, and returns its value as a string.
func jsEval(t *testing.T, app core.App, src string) (string, error) {
	t.Helper()
	vm, err := jsvm.New(jsvm.VMConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vm.Close)
	var out string
	err = vm.Run(func(rt js.Runtime) error {
		wasm.BindJS(app, rt)
		v, err := rt.Eval(src, "test.js")
		if err == nil {
			out = v.String()
		}
		return err
	})
	return out, err
}
