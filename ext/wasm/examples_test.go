package wasm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/wasm"
)

// rustModule builds a Rust plugin folder (cargo, wasm32-wasip1) and returns its .wasm; it skips the test when the
// toolchain isn't there.
func rustModule(t *testing.T, src, name string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm plugins")
	}
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("no cargo")
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if b, ok := built[src]; ok {
		return b
	}
	cmd := exec.Command("cargo", "build", "--release", "--target", "wasm32-wasip1")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cargo build (is the wasm32-wasip1 target installed? rustup target add wasm32-wasip1): %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(src, "target/wasm32-wasip1/release", name+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	built[src] = b
	return b
}

// zigModule builds a Zig plugin folder (zig build, Zig 0.14+) and returns its .wasm; it skips the test when there's
// no zig on the PATH or in ~/.local/zig-*.
func zigModule(t *testing.T, src, name string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm plugins")
	}
	zig, err := exec.LookPath("zig")
	if err != nil {
		home, _ := os.UserHomeDir()
		found, _ := filepath.Glob(filepath.Join(home, ".local", "zig-*", "zig"))
		if len(found) == 0 {
			t.Skip("no zig")
		}
		zig = found[len(found)-1]
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if b, ok := built[src]; ok {
		return b
	}
	cmd := exec.Command(zig, "build", "-Doptimize=ReleaseSmall")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("zig build (Zig 0.14+ needed): %v\n%s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(src, "zig-out/bin", name+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	built[src] = b
	return b
}

// checkExamples runs every example in the plugin's manifest and checks it gives the output it says.
func checkExamples(t *testing.T, e env, name string) {
	t.Helper()
	i, _ := info(t, e, name)
	if len(i.Examples) == 0 {
		t.Fatalf("%s: no examples", name)
	}
	for n, ex := range i.Examples {
		out, err := e.h.Call(context.Background(), name, ex.Function, ex.Stdin())
		if err != nil {
			t.Errorf("%s example %d (%s): %v", name, n, ex.Function, err)
			continue
		}
		if len(ex.Output) == 0 {
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
	src, want := "example/summary", "Go" // Go's module says it's Go; the others say it in their plugin.json
	var code []byte
	switch lang {
	case "rust":
		src, want = "example/summary-rust", "Rust"
		code = rustModule(t, src, "summary")
	case "zig":
		src, want = "example/summary-zig", "Zig"
		code = zigModule(t, src, "summary")
	default:
		code = module(t, src)
	}
	start := time.Now()
	e := newApp(t, wasm.Config{}, func(app core.App, dir string) {
		install(t, dir, "summary", code, manifest(t, src))
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
		install(t, dir, "evil", code, map[string]any{"functions": []string{"run", "listen", "exit"}})
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
