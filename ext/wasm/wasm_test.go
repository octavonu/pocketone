package wasm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"ganigeorgiev.com/pocketbase/pocketbase/apis"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tests"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/wasm"
)

const basic = "testdata/basic"

// install puts a module in the module folder, as a copy, a deploy or an SFTP upload would: <name>.pb.wasm.
func install(t *testing.T, dir, name string, code []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".pb.wasm"), code, 0o644); err != nil {
		t.Fatal(err)
	}
}

type env struct {
	app *tests.TestApp
	h   *wasm.Host
	dir string // the module folder
}

// newApp is an app with ext/wasm, serving, its module folder a fresh temp dir (unless cfg says otherwise). before
// runs ahead of serving; modules put in the folder later are there for the next call too.
func newApp(t *testing.T, cfg wasm.Config, before func(app core.App, dir string)) env {
	t.Helper()
	app, err := tests.NewTestApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	h := wasm.MustRegister(app, cfg)
	if before != nil {
		before(app, cfg.Dir)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	se := &core.ServeEvent{App: app, Router: r, Server: &http.Server{}}
	if err := app.OnServe().Trigger(se, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		e.Server.Handler = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.OnTerminate().Trigger(&core.TerminateEvent{App: app}) })
	return env{app: app, h: h, dir: cfg.Dir}
}

// withBasic is an app serving the basic test module as "basic".
func withBasic(t *testing.T, cfg wasm.Config) env {
	t.Helper()
	code := module(t, basic)
	return newApp(t, cfg, func(app core.App, dir string) {
		install(t, dir, "basic", code)
	})
}

// info is how the module called name is doing; ok false if the folder has none.
func info(t *testing.T, e env, name string) (wasm.Info, bool) {
	t.Helper()
	for _, i := range e.h.Modules() {
		if i.Name == name {
			return i, true
		}
	}
	return wasm.Info{}, false
}

func call(t *testing.T, e env, name, fn string, input any) (map[string]any, *wasm.Error) {
	t.Helper()
	in, _ := json.Marshal(input)
	out, err := e.h.Call(context.Background(), name, fn, in)
	if err != nil {
		var we *wasm.Error
		if !errors.As(err, &we) {
			t.Fatalf("%s.%s: error isn't a *wasm.Error: %v", name, fn, err)
		}
		return nil, we
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%s.%s: output isn't JSON: %q", name, fn, out)
	}
	return m, nil
}

func wantCode(t *testing.T, err *wasm.Error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %s, got none", code)
	}
	if err.Code != code {
		t.Fatalf("want error %s, got %s: %s", code, err.Code, err.Message)
	}
}

// TestInputOutput: the input goes to the module's stdin as JSON, and what it writes to stdout comes back.
func TestInputOutput(t *testing.T) {
	e := withBasic(t, wasm.Config{})
	out, err := call(t, e, "basic", "run", map[string]string{"text": "hello wasm world"})
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "HELLO WASM WORLD" || out["words"] != float64(3) {
		t.Fatalf("got %v", out)
	}
}

// TestTruncate: a module's message, cut to the size limit, is cut before a character, never inside one: in UTF-8,
// "ă" is 2 bytes and "😀" 4, and half a character would reach JS as "�".
func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"abă", 4, "abă"},
		{"abă", 3, "ab"}, // the limit falls inside ă
		{"a😀b", 5, "a😀"},
		{"a😀b", 4, "a"}, // inside 😀, at each of its bytes
		{"a😀b", 3, "a"},
		{"a😀b", 2, "a"},
		{"ăa", 1, ""},
		{strings.Repeat("ă", 600), 1 << 10, strings.Repeat("ă", 512)},             // a message at maxMessage, on a boundary
		{"x" + strings.Repeat("ă", 600), 1 << 10, "x" + strings.Repeat("ă", 511)}, // and off one
	} {
		got := wasm.Truncate(tc.s, tc.n)
		if got != tc.want || len(got) > tc.n || !utf8.ValidString(got) {
			t.Errorf("Truncate(%.12q…, %d) = %.12q… (%d bytes), want %.12q… (%d bytes)", tc.s, tc.n, got, len(got), tc.want, len(tc.want))
		}
	}
}

// TestErrors: every way a call can fail gives a WasmError with its code: the module's own code and message (exit
// non-zero with {"code", "message"} on stderr), module_error for a plain message or none, crashed for a panic,
// no_such_function from a module that doesn't have the function, not_found for a missing module. The module still
// works after all of them.
func TestErrors(t *testing.T) {
	e := withBasic(t, wasm.Config{})

	_, err := call(t, e, "basic", "fail", nil)
	wantCode(t, err, "rate_limited")
	if err.Message != "try again later" || !err.Retryable || err.Module != "basic" {
		t.Fatalf("structured error: %+v", err)
	}

	_, err = call(t, e, "basic", "failPlain", nil)
	wantCode(t, err, wasm.CodeModuleError)
	if err.Message != "something broke" || err.Retryable {
		t.Fatalf("plain error: %+v", err)
	}

	_, err = call(t, e, "basic", "failSilent", nil)
	wantCode(t, err, wasm.CodeModuleError)
	if err.Message != "exited with code 7" {
		t.Fatalf("silent error: %+v", err)
	}

	_, err = call(t, e, "basic", "panics", nil)
	wantCode(t, err, wasm.CodeCrashed)
	if err.Message != "panic: boom" {
		t.Fatalf("panic: %q", err.Message)
	}

	_, err = call(t, e, "basic", "run", "not an object")
	wantCode(t, err, wasm.CodeModuleError)

	_, err = call(t, e, "basic", "nosuch", nil) // the module's own answer, by convention
	wantCode(t, err, wasm.CodeNoSuchFunction)

	_, err = call(t, e, "nope", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)

	// the module still works after all that
	if _, err := call(t, e, "basic", "run", map[string]string{"text": "ok"}); err != nil {
		t.Fatal(err)
	}
}

// TestStdio: the input reaches stdin byte for byte, the output is stdout as is, the function is the first argument
// after the module's name, and stderr of a call that works is its log, not an error.
func TestStdio(t *testing.T) {
	e := withBasic(t, wasm.Config{})
	raw := []byte("UNB+UNOA:2+sender+receiver'\x00\xff\n")
	out, err := e.h.Call(context.Background(), "basic", "echo", raw)
	if err != nil || !bytes.Equal(out, raw) {
		t.Fatalf("echo: %q, %v", out, err)
	}
	out, err = e.h.Call(context.Background(), "basic", "args", nil)
	if err != nil || strings.TrimSpace(string(out)) != `["basic","args"]` {
		t.Fatalf("args: %s, %v", out, err)
	}
	out, err = e.h.Call(context.Background(), "basic", "logs", nil)
	if err != nil || string(out) != "done" {
		t.Fatalf("logs: %q, %v", out, err)
	}
}

// TestTimeout: a module that never ends is stopped at Timeout (well before the test's own limit), as a retryable
// timeout; a shorter deadline from the caller stops it too.
func TestTimeout(t *testing.T) {
	e := withBasic(t, wasm.Config{Timeout: 300 * time.Millisecond})
	start := time.Now()
	_, err := call(t, e, "basic", "loop", nil)
	wantCode(t, err, wasm.CodeTimeout)
	if !err.Retryable {
		t.Fatal("a timeout should be retryable")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stopped after %s", d)
	}

	// the caller's context counts too
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, cerr := e.h.Call(ctx, "basic", "loop", nil)
	var we *wasm.Error
	if !errors.As(cerr, &we) || we.Code != wasm.CodeTimeout {
		t.Fatalf("caller's deadline: %v", cerr)
	}
}

// TestMemoryLimit: a module can't use more memory than MaxCallMemoryMB (32 MB by default): a call that tries fails as
// crashed.
func TestMemoryLimit(t *testing.T) {
	e := withBasic(t, wasm.Config{}) // the default cap, 32 MB
	_, err := call(t, e, "basic", "grab", nil)
	wantCode(t, err, wasm.CodeCrashed)
	if !strings.Contains(err.Message, "each call may allocate up to 32 MB: MaxCallMemoryMB") {
		t.Fatalf("doesn't say which limit: %s", err.Message)
	}
}

// TestPayloadLimit: an input or an output over MaxPayload fails as too_large.
func TestPayloadLimit(t *testing.T) {
	e := withBasic(t, wasm.Config{MaxPayload: 512 << 10})
	_, err := call(t, e, "basic", "big", nil) // 1 MB out
	wantCode(t, err, wasm.CodeTooLarge)
	_, err = call(t, e, "basic", "run", map[string]string{"text": strings.Repeat("a ", 300<<10)}) // 600 KB in
	wantCode(t, err, wasm.CodeTooLarge)
}

// TestIsolation: every call gets a fresh instance, so a module can't keep one caller's input and hand it to the next.
// (The program starts over each time: a global set by one call isn't there for the next.)
func TestIsolation(t *testing.T) {
	e := withBasic(t, wasm.Config{})
	for i := range 3 {
		out, err := call(t, e, "basic", "counter", nil)
		if err != nil {
			t.Fatal(err)
		}
		if out["count"] != float64(1) {
			t.Fatalf("call %d: count = %v, want 1 (state survived between calls)", i+1, out["count"])
		}
	}
}

// TestConcurrentCalls: calls running at once each get a fresh instance, so none sees another's input or output
// (each counts its own words). MaxModulesMB is raised so the Go module (4.3 MB) runs many at once.
func TestConcurrentCalls(t *testing.T) {
	e := withBasic(t, wasm.Config{MaxModulesMB: 200})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text := strings.Repeat("w ", i+1)
			in, _ := json.Marshal(map[string]string{"text": text})
			out, err := e.h.Call(context.Background(), "basic", "run", in)
			if err != nil {
				errs <- err
				return
			}
			var m map[string]any
			json.Unmarshal(out, &m)
			if m["words"] != float64(i+1) {
				errs <- errors.New("wrong output: " + string(out))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestLoadAndStatus: every <name>.pb.wasm in the folder is a module, with what it says for --describe or, when it
// doesn't answer, the defaults, and Modules lists it; the JS hooks, other files, an upload in progress (a hidden
// file) and a name pb_hooks can't call are left out. A missing module's error names the file it looked for.
func TestLoadAndStatus(t *testing.T) {
	code := module(t, basic)
	plain := moduleTags(t, basic, "nodescribe")
	e := newApp(t, wasm.Config{}, func(app core.App, dir string) {
		install(t, dir, "basic", code)
		install(t, dir, "plain", plain) // doesn't answer --describe: just "run"
		os.WriteFile(filepath.Join(dir, "main.pb.js"), []byte("// the app's JS hooks"), 0o644)
		os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a module"), 0o644)
		os.WriteFile(filepath.Join(dir, "lib.wasm"), code, 0o644)                  // a .wasm, not a .pb.wasm
		os.WriteFile(filepath.Join(dir, ".basic.pb.wasm.part"), code[:100], 0o644) // an upload in progress
		os.WriteFile(filepath.Join(dir, "two words.pb.wasm"), code, 0o644)         // not a name pb_hooks can call
	})
	got := e.h.Modules()
	if len(got) != 2 || got[0].Name != "basic" || got[1].Name != "plain" {
		t.Fatalf("modules: %+v", got)
	}
	if !got[0].Running || got[0].Error != "" || got[0].Description == "" || got[0].Version != "1.0.0" || len(got[0].Functions) < 2 {
		t.Fatalf("basic: %+v", got[0])
	}
	if !got[1].Running || got[1].Error != "" || len(got[1].Functions) != 1 || got[1].Functions[0] != "run" {
		t.Fatalf("plain: %+v", got[1])
	}
	if !runs(t, e, "plain") {
		t.Fatal("plain didn't run")
	}
	if _, err := call(t, e, "plain", "counter", nil); err != nil { // not in the defaults, but the module has it
		t.Fatal(err)
	}
	out, err := e.h.Call(context.Background(), "basic", "--describe", nil) // what $wasm.call.async(name, "--describe") gives
	if err != nil || !strings.Contains(string(out), `"version": "1.0.0"`) {
		t.Fatalf("--describe: %s, %v", out, err)
	}
	_, werr := call(t, e, "nope", "run", nil)
	wantCode(t, werr, wasm.CodeNotFound)
	if !strings.Contains(werr.Message, "nope.pb.wasm") {
		t.Fatalf("message doesn't say which file: %s", werr.Message)
	}
}

// TestNotLoadedBeforeServing: modules run only while the app serves; before that (migrate, superuser and other
// commands) a call is not_loaded.
func TestNotLoadedBeforeServing(t *testing.T) {
	code := module(t, basic)
	app, err := tests.NewTestApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	dir := t.TempDir()
	install(t, dir, "basic", code)
	h := wasm.MustRegister(app, wasm.Config{Dir: dir})
	_, cerr := h.Call(context.Background(), "basic", "run", []byte(`{}`))
	var we *wasm.Error
	if !errors.As(cerr, &we) || we.Code != wasm.CodeNotLoaded {
		t.Fatalf("before serving: %v", cerr)
	}
}

// TestDefaultDir: the module folder is pb_hooks beside pb_data: modules are <name>.pb.wasm beside the JS hooks (where
// pocker's instance folders and `pocketone deploy` put them).
func TestDefaultDir(t *testing.T) {
	app, err := tests.NewTestApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	h := wasm.MustRegister(app, wasm.Config{})
	if want := filepath.Join(filepath.Dir(app.DataDir()), "pb_hooks"); h.Dir() != want {
		t.Fatalf("dir %s, want %s", h.Dir(), want)
	}
}

// TestSettings: an app's settings layers (YAML's `extensions.wasm`) go over the Config given to New, key by key, and
// `timeout` reads as "300ms" as well as nanoseconds.
func TestSettings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layers extkit.Config
		want   wasm.Config
	}{
		{"none", nil, wasm.Config{MaxCallMemoryMB: 64, MaxConcurrent: 3, MaxModulesMB: 10, Timeout: 2 * time.Second, MaxPayload: 4 << 20}},
		{"duration string", extkit.Config{{"timeout": "300ms", "maxCallMemoryMB": 16, "maxModulesMB": 40}},
			wasm.Config{MaxCallMemoryMB: 16, MaxConcurrent: 3, MaxModulesMB: 40, Timeout: 300 * time.Millisecond, MaxPayload: 4 << 20}},
		{"nanoseconds, later layer wins", extkit.Config{{"timeout": "1s", "maxPayload": 1024}, {"timeout": 1e9 / 2}},
			wasm.Config{MaxCallMemoryMB: 64, MaxConcurrent: 3, MaxModulesMB: 10, Timeout: 500 * time.Millisecond, MaxPayload: 1024}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, err := tests.NewTestApp(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer app.Cleanup()
			w := wasm.New(wasm.Config{MaxCallMemoryMB: 64, MaxConcurrent: 3, Timeout: 2 * time.Second})
			if err := w.Register(app, tc.layers); err != nil {
				t.Fatal(err)
			}
			got := wasm.For(app).Config()
			tc.want.Dir = got.Dir // the default folder, checked in TestDefaultDir
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	app, err := tests.NewTestApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	if err := wasm.New(wasm.Config{}).Register(app, extkit.Config{{"timeout": "five seconds"}}); err == nil {
		t.Fatal("a bad timeout registered")
	}
}

// TestLanguage: what a module's producers section says wins over the manifest; without one, the manifest's language
// shows. Examples come through with their function defaulted to run.
func TestLanguage(t *testing.T) {
	code := moduleTags(t, basic, "claimrust") // its --describe says Rust
	e := newApp(t, wasm.Config{}, func(_ core.App, dir string) {
		install(t, dir, "basic", code)
		install(t, dir, "nosection", says(`{"language": "Zig"}`))
	})
	if i, _ := info(t, e, "nosection"); i.Language != "Zig" {
		t.Errorf("no producers section: language %q, want the manifest's Zig", i.Language)
	}
	i, _ := info(t, e, "basic")
	if i.Language != "Go" {
		t.Errorf("language %q, want the module's Go", i.Language)
	}
	if i.Repository != "https://github.com/pockethost/pocketone/tree/main/ext/wasm/testdata/basic" {
		t.Errorf("repository %q", i.Repository)
	}
	if len(i.Examples) != 3 || i.Examples[0].Function != "run" || string(i.Examples[1].Stdin()) != "hi" ||
		string(i.Examples[2].Stdin()) != `{"a":1}` {
		t.Errorf("examples: %+v", i.Examples)
	}
	checkExamples(t, e, "basic")

	for _, tc := range []struct {
		name    string
		section []byte
		want    string
	}{
		{"go", producers("language", "Go", "go1.27.4", "processed-by", "Go cmd/compile", "go1.27.4"), "Go"},
		{"rust, after its libc", producers("language", "C11", "", "Rust", "", "processed-by", "rustc", "1.100.0"), "Rust"},
		{"zig: only its libc", producers("language", "C99", ""), ""},
		{"c++ only", producers("language", "C++", ""), ""},
		{"no language field", producers("processed-by", "clang", "23"), ""},
		{"truncated", producers("language", "Rust", "")[:6], ""},
		{"empty", nil, ""},
	} {
		if got := wasm.ProducersLanguage(tc.section); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestMaxConcurrent: no more than MaxConcurrent calls run at once; the next one waits, and gives up as busy.
// MaxModulesMB is raised so only MaxConcurrent limits (TestMaxModulesMB checks the other).
func TestMaxConcurrent(t *testing.T) {
	e := withBasic(t, wasm.Config{MaxConcurrent: 2, MaxModulesMB: 200, Timeout: 2 * time.Second})
	if _, err := call(t, e, "basic", "run", map[string]string{"text": "x"}); err != nil { // into the test cache
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 { // both slots taken by calls that run until the time limit
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.h.Call(context.Background(), "basic", "loop", nil)
		}()
	}
	time.Sleep(500 * time.Millisecond) // a call holds its slot while it compiles too: the loops end at compile + Timeout

	// the caller gives up first
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := e.h.Call(ctx, "basic", "run", []byte(`{"text":"x"}`))
	var we *wasm.Error
	if !errors.As(err, &we) || we.Code != wasm.CodeBusy || !we.Retryable {
		t.Fatalf("third call: %v", err)
	}

	// one that waits gets a slot when the loops end
	start := time.Now()
	if _, err := e.h.Call(context.Background(), "basic", "run", []byte(`{"text":"x"}`)); err != nil {
		t.Fatalf("waiting call: %v", err)
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("ran after %s: didn't wait for a slot", d)
	}
	wg.Wait()
}

// TestCold: as outside the tests, nothing compiled is kept, so every call compiles the module. Compiling isn't
// counted in Timeout: a Go module takes longer than 300 ms to compile and still runs.
func TestCold(t *testing.T) {
	wasm.Cold(t)
	e := withBasic(t, wasm.Config{Timeout: 300 * time.Millisecond})
	for i := range 2 {
		start := time.Now()
		out, err := call(t, e, "basic", "run", map[string]string{"text": "x"})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		t.Logf("call %d: %v in %s", i, out, time.Since(start).Round(time.Millisecond))
		if d := time.Since(start); d < 300*time.Millisecond {
			t.Fatalf("call %d took %s: was the module compiled before?", i, d)
		}
	}
	_, err := call(t, e, "basic", "loop", nil)
	wantCode(t, err, wasm.CodeTimeout)
}
