package wasm_test

// Plugins that must be refused, or run with the defaults, and say why: a bad --describe, a module that isn't valid
// WebAssembly, one over MaxModulesMB, and modules that aren't WASI programs (host functions this host doesn't have,
// a library). The lab has the same set: broken, fat_evil, phone, library.

import (
	"context"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/wasm"
)

// TestBadManifests: when what a plugin prints for --describe isn't a manifest, or asks for more than a pure function
// gets, Plugins says why, and the plugin runs with the defaults (calls don't read the manifest).
func TestBadManifests(t *testing.T) {
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
		{"the host's flag as a function", `{"functions":["--describe"]}`, "valid function name"},
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
			e := newApp(t, wasm.Config{}, func(_ core.App, dir string) { install(t, dir, "bad", says(tc.value)) })
			i, _ := info(t, e, "bad")
			if !i.Running || !strings.HasPrefix(i.Error, "--describe: ") || !strings.Contains(i.Error, tc.want) ||
				len(i.Functions) != 1 || i.Functions[0] != "run" || i.Description != "" {
				t.Fatalf("%+v, want %q", i, tc.want)
			}
			if _, err := e.h.Call(context.Background(), "bad", "run", nil); err != nil {
				t.Fatalf("the call: %v", err)
			}
		})
	}
}

// TestBrokenModule: a module that isn't valid WebAssembly doesn't load and says why, and the other plugins load as
// usual.
func TestBrokenModule(t *testing.T) {
	code := module(t, basic)
	e := newApp(t, wasm.Config{}, func(_ core.App, dir string) {
		install(t, dir, "broken", code[:1000])
		install(t, dir, "basic", code)
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
		install(t, dir, "big", make([]byte, 2<<20))
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
			e := newApp(t, wasm.Config{}, func(_ core.App, dir string) { install(t, dir, "phone", code) })
			if i, _ := info(t, e, "phone"); !strings.Contains(i.Error, tc.want) {
				t.Fatalf("%+v, want %q", i, tc.want)
			}
			_, err := call(t, e, "phone", "run", nil)
			wantCode(t, err, wasm.CodeNotLoaded)
		})
	}
}
