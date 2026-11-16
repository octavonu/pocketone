package wasm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/wasm"
)

// checkExamples runs every example in the plugin's manifest and checks it gives the output it says. An example that
// says no output may fail: it shows a failure (memhog's 64 MB).
func checkExamples(t *testing.T, e env, name string) {
	t.Helper()
	i, _ := info(t, e, name)
	if len(i.Examples) == 0 {
		t.Fatalf("%s: no examples", name)
	}
	for n, ex := range i.Examples {
		out, err := e.h.Call(context.Background(), name, ex.Function, ex.Stdin())
		if len(ex.Output) == 0 {
			continue
		}
		if err != nil {
			t.Errorf("%s example %d (%s): %v", name, n, ex.Function, err)
			continue
		}
		var got, want any
		if json.Unmarshal(out, &got) != nil {
			got = string(out)
		}
		if err := json.Unmarshal(ex.Output, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s example %d (%s): got %s, want %s", name, n, ex.Function, out, ex.Output)
		}
	}
}

// TestSummaryExample: the summary plugin, in Go, Rust and Zig, gives the same answers.
func TestSummaryExample(t *testing.T) {
	for _, lang := range []string{"go", "rust", "zig"} {
		t.Run(lang, func(t *testing.T) { testSummary(t, lang) })
	}
}

func testSummary(t *testing.T, lang string) {
	src, want := "example/summary", "Go" // Go's module says it's Go; the others say it in their --describe
	var code []byte
	switch lang {
	case "rust":
		src, want = "example/summary-rust", "Rust"
		code = prebuiltModule(t, src)
	case "zig":
		src, want = "example/summary-zig", "Zig"
		code = prebuiltModule(t, src)
	default:
		code = module(t, src)
	}
	start := time.Now()
	e := newApp(t, wasm.Config{}, func(app core.App, dir string) {
		install(t, dir, "summary", code)
	})
	t.Logf("%s: module %d KB, checked and loaded in %s", lang, len(code)>>10, time.Since(start).Round(time.Millisecond))
	if i, _ := info(t, e, "summary"); i.Language != want {
		t.Errorf("language %q, want %q", i.Language, want)
	}
	checkExamples(t, e, "summary")
	out, err := call(t, e, "summary", "run", map[string]string{
		"title": "Hello", "body": "one two three four five six seven eight nine ten eleven twelve thirteen"})
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "Hello: one two three four five six seven eight nine ten eleven twelve …" || out["words"] != float64(13) {
		t.Fatalf("got %v", out)
	}
	_, err = call(t, e, "summary", "run", map[string]string{"title": "Empty"})
	wantCode(t, err, "empty_body")
	_, err = call(t, e, "summary", "other", nil) // the plugin answers a function it doesn't have
	wantCode(t, err, wasm.CodeNoSuchFunction)
	if i, _ := info(t, e, "summary"); i.Error != "" || i.Description == "" || len(i.Functions) != 1 {
		t.Errorf("--describe: %+v", i)
	}

	start = time.Now()
	for range 100 {
		if _, err := call(t, e, "summary", "run", map[string]string{"title": "t", "body": "a b c"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%s: %s per call, run only (the test cache skips compiling)", lang, (time.Since(start) / 100).Round(10*time.Microsecond))

	wasm.Cold(t) // as outside the tests: each call compiles
	start = time.Now()
	for range 3 {
		if _, err := call(t, e, "summary", "run", map[string]string{"title": "t", "body": "a b c"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%s: %s per call, compiling each time", lang, (time.Since(start) / 3).Round(100*time.Microsecond))
}

var update = flag.Bool("update", false, "rebuild the prebuilt Rust and Zig examples (example/*/*.pb.wasm) from their sources")

// TestPrebuiltExamples: every Rust and Zig example, as kept built beside its source, loads, describes itself as its
// plugin.json says (so the build isn't older than its manifest), and gives what each of its examples says. They run
// with no Rust or Zig installed, so a change to ext/wasm that breaks a real plugin shows here. With -update, each is
// rebuilt from its source first (with rustVersion and zigVersion).
func TestPrebuiltExamples(t *testing.T) {
	for _, p := range prebuilt {
		t.Run(filepath.Base(p.dir), func(t *testing.T) {
			path := filepath.Join(p.dir, p.file)
			if *update {
				if err := os.WriteFile(path, buildExample(t, p.dir), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code := prebuiltModule(t, p.dir)
			name := strings.TrimSuffix(p.file, ".pb.wasm")
			e := newApp(t, wasm.Config{}, func(_ core.App, dir string) { install(t, dir, name, code) })
			if i, _ := info(t, e, name); !i.Running || i.Error != "" {
				t.Fatalf("%+v", i)
			}
			got, err := e.h.Call(context.Background(), name, "--describe", nil)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(p.dir, "plugin.json"))
			if err != nil {
				t.Fatal(err)
			}
			var g, w any
			if json.Unmarshal(got, &g) != nil || json.Unmarshal(want, &w) != nil || !reflect.DeepEqual(g, w) {
				t.Fatalf("--describe isn't %s/plugin.json: rebuild it (go test -run TestPrebuiltExamples -update)\n%s", p.dir, got)
			}
			checkExamples(t, e, name)
		})
	}
}

// TestPrebuiltMatchSources: each kept build is what its source builds, byte for byte. It needs Rust rustVersion
// (with wasm32-wasip1) or Zig zigVersion, and skips the others.
func TestPrebuiltMatchSources(t *testing.T) {
	for _, p := range prebuilt {
		t.Run(filepath.Base(p.dir), func(t *testing.T) {
			if !bytes.Equal(buildExample(t, p.dir), prebuiltModule(t, p.dir)) {
				t.Fatalf("%s isn't what its source builds: rebuild it (go test -run TestPrebuiltExamples -update)", filepath.Join(p.dir, p.file))
			}
		})
	}
}

// TestHostilePlugin: a plugin that tries every way out of the box gets nothing, and can't take the host down.
func TestHostilePlugin(t *testing.T) {
	src := "testdata/evil"
	code := module(t, src)
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer srv.Close()
	os.Setenv("EVIL_TEST_SECRET", "s3cret") // in the host's environment: the plugin must not see it
	defer os.Unsetenv("EVIL_TEST_SECRET")

	// it imports net/http (to try the network), so its module is ~11 MB: over the default MaxModulesMB
	e := newApp(t, wasm.Config{Timeout: 5 * time.Second, MaxModulesMB: 20}, func(app core.App, dir string) {
		install(t, dir, "evil", code)
	})
	target := strings.TrimPrefix(srv.URL, "http://")
	for i := range 2 { // twice: the second call would see what the first left behind
		in, _ := json.Marshal(map[string]string{"target": target})
		out, err := e.h.Call(context.Background(), "evil", "run", in)
		if err != nil {
			t.Fatal(err)
		}
		var report map[string]string
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatalf("report: %s", out)
		}
		keys := make([]string, 0, len(report))
		for k := range report {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("call %d: %-34s %s", i+1, k, report[k])
			if k == "hostname" && report[k] == "GOT: wasip1" {
				continue // Go's os.Hostname on wasip1 is that constant, not the machine's name
			}
			if strings.HasPrefix(report[k], "GOT") {
				t.Errorf("call %d: %s: %s", i+1, k, report[k])
			}
		}
		if strings.Contains(string(out), "s3cret") {
			t.Error("the host's environment leaked")
		}
	}
	if reached {
		t.Error("the plugin reached the test server")
	}

	// Go's net.Listen works on wasip1, inside the program only (its in-memory network): nothing outside reaches it
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := free.Addr().String()
	free.Close()
	done := make(chan string)
	go func() {
		in, _ := json.Marshal(map[string]string{"addr": addr})
		out, err := e.h.Call(context.Background(), "evil", "listen", in)
		done <- fmt.Sprint(string(out), err)
	}()
	time.Sleep(300 * time.Millisecond) // the plugin is listening now
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("reached the plugin's listener at %s from outside", addr)
	}
	got := <-done
	t.Logf("listen: %s", got)
	if strings.HasPrefix(got, "GOT") {
		t.Error(got)
	}

	// exiting ends the call, not the host
	start := time.Now()
	_, exitErr := e.h.Call(context.Background(), "evil", "exit", nil)
	t.Logf("exit: %v (%s)", exitErr, time.Since(start))
	if _, err := e.h.Call(context.Background(), "evil", "run", fmt.Appendf(nil, `{"target":%q}`, target)); err != nil {
		t.Fatalf("after exit: %v", err)
	}
}
