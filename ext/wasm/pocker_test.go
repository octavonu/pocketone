package wasm_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pockethost/pocketone/ext/pocker"
	"github.com/pockethost/pocketone/ext/standard"
	"github.com/pockethost/pocketone/ext/wasm"
)

type instances []pocker.Instance

func (s instances) Snapshot(context.Context) ([]pocker.Instance, error) { return s, nil }
func (s instances) Watch(context.Context) (<-chan pocker.Change, error) { return nil, nil }

// probe is a pb_hooks route that tells what $wasm looks like in an instance, and calls a plugin both ways.
const probe = `
routerAdd("GET", "/probe", async (e) => {
  if (typeof $wasm === "undefined") return e.json(200, { wasm: "undefined" })
  const name = e.request.url.query().get("plugin")
  const out = { wasm: typeof $wasm, limits: $wasm.limits(), plugins: $wasm.plugins().map((p) => p.name) }
  try {
    out.sync = $wasm.run(name, { text: "hi pocker" }).text
    out.async = (await $wasm.run.async(name, { text: "hi pocker" })).text
  } catch (err) {
    out.error = err.code
  }
  return e.json(200, out)
})`

// TestPocker: ext/wasm in the standard set under pocker. One extension value serves every instance, and each
// instance has its own plugins (its pb_wasm/), its own settings (extensions.wasm), and $wasm only when it's on; an
// instance stopped and started again runs its plugins as before, and nothing is left running after the host stops.
func TestPocker(t *testing.T) {
	code, m := module(t, basic), manifest(t, basic)

	root := t.TempDir()
	for id, plugin := range map[string]string{"a": "basic", "b": "other", "c": "basic"} {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(filepath.Join(dir, "pb_hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pb_hooks", "probe.pb.js"), []byte(probe), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "pb_wasm"), 0o755); err != nil {
			t.Fatal(err)
		}
		install(t, filepath.Join(dir, "pb_wasm"), plugin, code, m)
	}

	mgr := pocker.NewManager(instances{
		{ID: "a", Hosts: []string{"a.test"}, Extensions: map[string]any{"wasm": map[string]any{"timeout": "10s", "maxCallMemoryMB": 64}}},
		{ID: "b", Hosts: []string{"b.test"}},
		{ID: "c", Hosts: []string{"c.test"}, Extensions: map[string]any{"wasm": map[string]any{"enabled": false}}},
	}, pocker.ManagerOptions{
		PBRoot:   root,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Standard: standard.New().Add(wasm.New(wasm.Config{})),
	})
	ctx, cancel := context.WithCancel(context.Background())
	go mgr.Run(ctx)
	for deadline := time.Now().Add(10 * time.Second); !mgr.Ready(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("manager not ready")
		}
	}

	type answer struct {
		Wasm    string
		Limits  wasm.Limits
		Plugins []string
		Sync    string
		Async   string
		Error   string
	}
	get := func(host, plugin string) answer {
		t.Helper()
		rec := httptest.NewRecorder()
		mgr.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/probe?plugin="+plugin, nil))
		var a answer
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &a) != nil {
			t.Fatalf("%s /probe: %d %s", host, rec.Code, rec.Body)
		}
		return a
	}
	check := func() {
		t.Helper()
		// each instance runs its own plugins, and only them
		if a := get("a.test", "basic"); a.Sync != "HI POCKER" || a.Async != "HI POCKER" || len(a.Plugins) != 1 || a.Plugins[0] != "basic" {
			t.Errorf("a: %+v", a)
		}
		if a := get("a.test", "other"); a.Error != wasm.CodeNotFound {
			t.Errorf("a calling b's plugin: %+v", a)
		}
		if b := get("b.test", "other"); b.Sync != "HI POCKER" || len(b.Plugins) != 1 || b.Plugins[0] != "other" {
			t.Errorf("b: %+v", b)
		}
		if b := get("b.test", "basic"); b.Error != wasm.CodeNotFound {
			t.Errorf("b calling a's plugin: %+v", b)
		}
		// its own settings, over the defaults
		if l := get("a.test", "basic").Limits; l.TimeoutMs != 10000 || l.MaxCallMemoryMB != 64 || l.MaxConcurrent != 8 {
			t.Errorf("a's limits: %+v", l)
		}
		if l := get("b.test", "other").Limits; l.TimeoutMs != 5000 || l.MaxCallMemoryMB != 32 {
			t.Errorf("b's limits: %+v", l)
		}
		// off: no $wasm at all, though its pb_wasm/ has a plugin
		if c := get("c.test", "basic"); c.Wasm != "undefined" {
			t.Errorf("c: %+v", c)
		}
	}
	check()

	// stopped and started again (as a deploy or a restore does): its plugins run as before
	if err := mgr.WithInstanceStopped(ctx, "a", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	check()

	// nothing of ext/wasm or wazero keeps running once the host has stopped (the standard set's own process-wide
	// resources, pitr's litestream stores, close later, with the host's Closers)
	mgr.Shutdown()
	cancel()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		buf := make([]byte, 1<<20)
		stacks := string(buf[:runtime.Stack(buf, true)])
		if !strings.Contains(stacks, "tetratelabs/wazero") && !strings.Contains(stacks, "pocketone/ext/wasm.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after the host stopped:\n%s", stacks)
		}
	}
}
