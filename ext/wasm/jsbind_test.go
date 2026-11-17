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
onRecordCreate(async (e) => {
  const out = await $wasm.run.async("basic", { text: e.record.get("title") })
  e.record.set("summary", out.text)
  e.record.set("words", out.words)
  return e.next()
}, "posts")

onRecordCreate(async (e) => {
  try {
    const out = await $wasm.call.async(e.record.get("plugin"), e.record.get("fn"), { text: "x" })
    e.record.set("result", JSON.stringify(out))
  } catch (err) {
    e.record.set("name", err.name)
    e.record.set("code", err.code)
    e.record.set("message", err.message)
    e.record.set("retryable", err.retryable)
    e.record.set("plugin", err.plugin)
  }
  e.record.set("plugins", JSON.stringify($wasm.plugins().filter((p) => p.running).map((p) => p.name)))
  return e.next()
}, "results")

onRecordCreate(async (e) => {
  await $wasm.run.async(e.record.get("plugin"), {})
  return e.next()
}, "strict")

// the plugin's stderr, through inspect
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

onRecordCreate(async (e) => {
  const inputs = {
    string: "UNB+UNOA:2'",
    bytes: new Uint8Array([85, 78, 66]),
    buffer: new Uint8Array([85, 78, 72]).buffer,
    view: new Uint8Array([0, 85, 78, 84, 0]).subarray(1, 4),
    none: null,
    object: { a: 1 },
  }
  const out = await $wasm.call.async("basic", "echo", inputs[e.record.get("kind")])
  e.record.set("result", typeof out + " " + JSON.stringify(out))
  return e.next()
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
// $wasm.call.async and $wasm.plugins(), every error as a WasmError rejection with its code, how inputs (string,
// bytes, buffer, null, object) and outputs (JSON, text, empty) cross over, and an uncaught error failing the save.
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

			// $wasm.call.async: another declared function, output parsed
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

			// another hook: the same results, and the plugin's stderr through inspect
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
					t.Errorf("%s.%s: %q %q, want %s", tc.plugin, tc.fn, r.GetString("result"), r.GetString("stderr"), tc.want)
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

// TestAsyncOnly: run, call and inspect are async only; called without .async, each throws and says how to call it.
func TestAsyncOnly(t *testing.T) {
	e := withBasic(t, wasm.Config{})
	for _, tc := range []struct{ src, want string }{
		{`$wasm.run("basic", {})`, "$wasm.run is async only: use await $wasm.run.async(name, input) in an async hook"},
		{`$wasm.call("basic", "run", {})`, "$wasm.call is async only: use await $wasm.call.async(name, function, input)"},
		{`$wasm.inspect("basic", "run", {})`, "$wasm.inspect is async only: use await $wasm.inspect.async(name, function, input)"},
	} {
		_, err := jsEval(t, e.app, tc.src)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.src, err)
		}
	}
	if out, err := jsEval(t, e.app, `$wasm.run.async("basic", { text: "still works" }).then((o) => o.text)`); err != nil || out != "STILL WORKS" {
		t.Fatalf("async: %q %v", out, err)
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
onRecordCreate(async (e) => { e.record.set("summary", (await $wasm.run.async("basic", { text: e.record.get("title") })).text); return e.next() }, "posts")`), 0o644)
	pluginDir := t.TempDir()
	w := wasm.New(wasm.Config{Dir: pluginDir})
	if err := jsvm.Register(app, jsvm.Config{
		HooksDir: dir,
		OnInit:   func(rt js.Runtime) { extkit.BindJS(app, rt, w) }, // the engine starts before MustRegister
	}); err != nil {
		t.Fatal(err)
	}
	extkit.MustRegister(app, w)

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

// TestInspect: $wasm.inspect.async never rejects for the call: it returns the output, what the plugin wrote to stderr, the
// error as plain fields, and how long compiling and running took.
func TestInspect(t *testing.T) {
	e := withBasic(t, wasm.Config{})
	text, err := jsEval(t, e.app, `Promise.all([
		$wasm.inspect.async("basic", "logs", ""),
		$wasm.inspect.async("basic", "fail", ""),
		$wasm.inspect.async("basic", "echo", { a: 1 }),
		$wasm.inspect.async("nope", "run", {}),
	]).then(JSON.stringify)`)
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

// jsEval runs src, an expression, on a JS VM with $wasm bound to app, and returns its value as a string, awaited
// when it's a promise.
func jsEval(t *testing.T, app core.App, src string) (string, error) {
	t.Helper()
	vm, err := jsvm.New(jsvm.VMConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vm.Close)
	var f js.Func
	err = vm.Run(func(rt js.Runtime) error {
		wasm.BindJS(app, rt)
		v, err := rt.Eval("(async () => "+src+")", "test.js")
		if err == nil {
			f = v.Func()
		}
		return err
	})
	if err != nil {
		return "", err
	}
	out, err := f.Call()
	return fmt.Sprint(out), err
}
