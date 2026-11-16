package wasm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/wasm"
)

func runs(t *testing.T, e env, name string) bool {
	t.Helper()
	_, err := call(t, e, name, "run", map[string]string{"text": "x"})
	return err == nil
}

// TestLifecycle: every call reads the folder as it is then, with no restart: added, replaced, broken, fixed, renamed,
// removed.
func TestLifecycle(t *testing.T) {
	code := module(t, basic)
	e := newApp(t, wasm.Config{}, nil)

	// added
	install(t, e.dir, "basic", code, nil)
	if !runs(t, e, "basic") {
		t.Fatal("didn't run once added")
	}
	_, err := call(t, e, "basic", "counter", nil)
	wantCode(t, err, wasm.CodeNoSuchFunction)

	// a manifest added: its functions callable
	install(t, e.dir, "basic", code, map[string]any{"functions": []string{"run", "counter"}})
	if _, err := call(t, e, "basic", "counter", nil); err != nil {
		t.Fatal(err)
	}

	// broken (or half uploaded): not loaded, and Plugins says why
	install(t, e.dir, "basic", code[:1000], map[string]any{"functions": []string{"run", "counter"}})
	if i, _ := info(t, e, "basic"); i.Running || !strings.HasPrefix(i.Error, "module:") {
		t.Fatalf("broken: %+v", i)
	}
	_, err = call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotLoaded)

	// a bad manifest: the same
	install(t, e.dir, "basic", code, `{"dependencies":{"openai":{"url":"https://api.openai.com"}}}`)
	if i, _ := info(t, e, "basic"); i.Running || !strings.Contains(i.Error, "basic.json") || !strings.Contains(i.Error, "unknown field") {
		t.Fatalf("bad manifest: %+v", i)
	}

	// fixed
	install(t, e.dir, "basic", code, manifest(t, basic))
	if i, _ := info(t, e, "basic"); !i.Running || i.Error != "" {
		t.Fatalf("fixed: %+v", i)
	}

	// renamed: callable by its new name only
	if err := os.Rename(filepath.Join(e.dir, "basic.wasm"), filepath.Join(e.dir, "upper.wasm")); err != nil {
		t.Fatal(err)
	}
	if !runs(t, e, "upper") {
		t.Fatal("not callable by its new name")
	}
	_, err = call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)

	// removed (how a plugin is turned off)
	if err := os.Remove(filepath.Join(e.dir, "upper.wasm")); err != nil {
		t.Fatal(err)
	}
	if got := e.h.Plugins(); len(got) != 0 {
		t.Fatalf("still there: %+v", got)
	}
	_, err = call(t, e, "upper", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)
}

// TestMakesTheFolder: a missing plugin folder is created when the app starts serving, and one that can't be made
// (here its parent is a file) doesn't stop the app from serving.
func TestMakesTheFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pb_wasm")
	newApp(t, wasm.Config{Dir: dir}, nil)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("not created: %v", err)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e := newApp(t, wasm.Config{Dir: filepath.Join(file, "pb_wasm")}, nil)
	if got := e.h.Plugins(); len(got) != 0 {
		t.Fatalf("plugins: %+v", got)
	}
}

// TestCallReadsTheFiles: a call reads the plugin's files as they are then, like `node script.js`: a plugin added,
// replaced or removed while the app serves is what the next call gets, and Plugins says so too; nothing stays in
// memory between calls, so a removed plugin can't be called any more.
func TestCallReadsTheFiles(t *testing.T) {
	code := module(t, basic)
	dir := filepath.Join(t.TempDir(), "pb_wasm") // not there yet: made at start
	e := newApp(t, wasm.Config{Dir: dir, Timeout: time.Second}, nil)

	install(t, dir, "basic", code, manifest(t, basic))
	if !runs(t, e, "basic") {
		t.Fatal("a plugin added while the app serves didn't run")
	}
	// replaced by another module: the next call runs that one
	install(t, dir, "basic", tiny(false, 0), nil)
	if _, err := e.h.Call(context.Background(), "basic", "run", nil); err != nil {
		t.Fatalf("the replaced plugin: %v", err)
	}
	if i, _ := info(t, e, "basic"); !i.Running || i.Size != len(tiny(false, 0)) || i.Language != "" {
		t.Fatalf("Plugins still says the old one: %+v", i)
	}
	os.Remove(filepath.Join(dir, "basic.wasm"))
	_, err := call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)
	if got := e.h.Plugins(); len(got) != 0 {
		t.Fatalf("still listed: %+v", got)
	}

	// a call in progress finishes with the module it read, whatever happens to the file
	install(t, dir, "spin", tiny(true, 0), nil)
	done := make(chan error, 1)
	go func() {
		_, err := e.h.Call(context.Background(), "spin", "run", nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	os.Remove(filepath.Join(dir, "spin.wasm"))
	var we *wasm.Error
	if err := <-done; !errors.As(err, &we) || we.Code != wasm.CodeTimeout {
		t.Fatalf("the call in progress: %v", err)
	}
}

// TestBadManifests: a manifest that isn't one, or that asks for more than a pure function gets, keeps the plugin
// from loading, and says why.
func TestBadManifests(t *testing.T) {
	code := module(t, basic)
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"not JSON", `{functions: run}`, "not a plugin manifest"},
		{"not an object", `[1]`, "not a plugin manifest"},
		{"more after the object", `{"description":"a"} {"description":"b"}`, "more after the JSON object"},
		{"a stray brace after it", "{\"description\":\"a\"}}\n", "more after the JSON object"},
		{"bad function name", `{"functions":["run now"]}`, "valid function name"},
		{"dependencies", `{"dependencies":{"openai":{"url":"https://api.openai.com"}}}`, "unknown field"},
		{"v1 permissions", `{"events":["posts.create"],"allow":{"posts":["view"]}}`, "unknown field"},
		{"bad language", `{"language":"Rust; rm -rf /"}`, "isn't a language name"},
		{"bad version", `{"version":"1.0 <script>"}`, "isn't a version"},
		{"example of an undeclared function", `{"examples":[{"function":"echo","input":"x"}]}`, `function "echo" isn't in functions`},
		{"example without input", `{"examples":[{"output":"x"}]}`, "no input"},
		{"too many examples", `{"examples":[` + strings.Repeat(`{"input":1},`, 16) + `{"input":1}]}`, "at most 16"},
		{"javascript repository", `{"repository":"javascript:alert(1)"}`, "isn't an http(s) URL"},
		{"repository with a password", `{"repository":"https://me:secret@github.com/x"}`, "isn't an http(s) URL"},
		{"repository with markup", `{"repository":"https://github.com/x\"><script>"}`, "isn't an http(s) URL"},
		{"too large", `{"description":"` + strings.Repeat("x", 64<<10) + `"}`, "over 64 KB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newApp(t, wasm.Config{}, func(_ core.App, dir string) { install(t, dir, "bad", code, tc.value) })
			if i, _ := info(t, e, "bad"); i.Running || !strings.Contains(i.Error, tc.want) {
				t.Fatalf("%+v, want %q", i, tc.want)
			}
			_, err := call(t, e, "bad", "run", nil)
			wantCode(t, err, wasm.CodeNotLoaded)
			if !strings.Contains(err.Message, tc.want) {
				t.Fatalf("message doesn't say why: %s", err.Message)
			}
		})
	}
}

// TestBrokenModule: a module that isn't valid WebAssembly doesn't load and says why, and the other plugins load as
// usual.
func TestBrokenModule(t *testing.T) {
	code := module(t, basic)
	m := manifest(t, basic)
	e := newApp(t, wasm.Config{}, func(_ core.App, dir string) {
		install(t, dir, "broken", code[:1000], m)
		install(t, dir, "basic", code, m)
	})
	if i, _ := info(t, e, "broken"); i.Running || !strings.HasPrefix(i.Error, "module:") {
		t.Errorf("broken: %+v", i)
	}
	if i, _ := info(t, e, "basic"); !i.Running || i.Error != "" {
		t.Errorf("good: %+v", i)
	}
	_, err := call(t, e, "broken", "run", nil)
	wantCode(t, err, wasm.CodeNotLoaded)
	if !strings.Contains(err.Message, "module:") {
		t.Errorf("message doesn't say why: %s", err.Message)
	}
	if !runs(t, e, "basic") {
		t.Error("the good plugin stopped")
	}
}

// TestModuleTooLarge: a .wasm over MaxModulesMB isn't read (it could never run), and Plugins and the call say why.
func TestModuleTooLarge(t *testing.T) {
	e := newApp(t, wasm.Config{MaxModulesMB: 1}, func(_ core.App, dir string) {
		install(t, dir, "big", make([]byte, 2<<20), nil)
	})
	if i, _ := info(t, e, "big"); i.Running || !strings.Contains(i.Error, "module of 2.0 MB, over MaxModulesMB (1 MB)") {
		t.Fatalf("%+v", i)
	}
	_, err := call(t, e, "big", "run", nil)
	wantCode(t, err, wasm.CodeNotLoaded)
	if !strings.Contains(err.Message, "module of 2.0 MB, over MaxModulesMB (1 MB)") {
		t.Fatalf("message: %s", err.Message)
	}
}

// TestUnknownImports: a module that isn't a plugin is refused when it loads, saying why: one built for host
// functions this host doesn't have (v1's pb_fetch, an Extism PDK's), or a library with no main to run.
func TestUnknownImports(t *testing.T) {
	for _, tc := range []struct {
		name string
		code func(t *testing.T) []byte
		want string
	}{
		{"pb_fetch", func(t *testing.T) []byte { return module(t, "testdata/imports") }, "imports extism:host/user.pb_fetch"},
		{"extism", func(t *testing.T) []byte { return moduleTags(t, "testdata/imports", "extism") }, "an Extism plugin"},
		{"library", func(t *testing.T) []byte { return build(t, basic, "", true) }, "not a WASI program (no _start)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code := tc.code(t)
			e := newApp(t, wasm.Config{}, func(_ core.App, dir string) { install(t, dir, "phone", code, nil) })
			if i, _ := info(t, e, "phone"); !strings.Contains(i.Error, tc.want) {
				t.Fatalf("%+v, want %q", i, tc.want)
			}
			_, err := call(t, e, "phone", "run", nil)
			wantCode(t, err, wasm.CodeNotLoaded)
		})
	}
}
