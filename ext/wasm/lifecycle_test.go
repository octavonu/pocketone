package wasm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// added; the module itself answers a function it doesn't have
	install(t, e.dir, "basic", code)
	if !runs(t, e, "basic") {
		t.Fatal("didn't run once added")
	}
	if i, _ := info(t, e, "basic"); !i.Running || i.Version != "1.0.0" {
		t.Fatalf("added: %+v", i)
	}
	_, err := call(t, e, "basic", "nosuch", nil)
	wantCode(t, err, wasm.CodeNoSuchFunction)

	// broken (or half uploaded): not loaded, and Modules says why
	install(t, e.dir, "basic", code[:1000])
	if i, _ := info(t, e, "basic"); i.Running || !strings.HasPrefix(i.Error, "module:") {
		t.Fatalf("broken: %+v", i)
	}
	_, err = call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotLoaded)

	// fixed
	install(t, e.dir, "basic", code)
	if i, _ := info(t, e, "basic"); !i.Running || i.Error != "" {
		t.Fatalf("fixed: %+v", i)
	}

	// renamed: callable by its new name only
	if err := os.Rename(filepath.Join(e.dir, "basic.pb.wasm"), filepath.Join(e.dir, "upper.pb.wasm")); err != nil {
		t.Fatal(err)
	}
	if !runs(t, e, "upper") {
		t.Fatal("not callable by its new name")
	}
	_, err = call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)

	// removed (how a module is turned off)
	if err := os.Remove(filepath.Join(e.dir, "upper.pb.wasm")); err != nil {
		t.Fatal(err)
	}
	if got := e.h.Modules(); len(got) != 0 {
		t.Fatalf("still there: %+v", got)
	}
	_, err = call(t, e, "upper", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)
}

// TestNoFolder: an app with no module folder (no pb_hooks) serves with no modules, and doesn't make one: the folder
// is the app's own, made by whoever writes its hooks.
func TestNoFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pb_hooks")
	e := newApp(t, wasm.Config{Dir: dir}, nil)
	if got := e.h.Modules(); len(got) != 0 {
		t.Fatalf("modules: %+v", got)
	}
	_, err := call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("made the folder")
	}
}

// TestCallReadsTheFiles: a call reads the module's files as they are then, like `node script.js`: a module added,
// replaced or removed while the app serves is what the next call gets, and Modules says so too; nothing stays in
// memory between calls, so a removed module can't be called any more.
func TestCallReadsTheFiles(t *testing.T) {
	code := module(t, basic)
	dir := filepath.Join(t.TempDir(), "pb_hooks") // not there yet when the app starts
	e := newApp(t, wasm.Config{Dir: dir, Timeout: time.Second}, nil)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	install(t, dir, "basic", code)
	if !runs(t, e, "basic") {
		t.Fatal("a module added while the app serves didn't run")
	}
	// replaced by another module: the next call runs that one
	install(t, dir, "basic", tiny(false, 0))
	if _, err := e.h.Call(context.Background(), "basic", "run", nil); err != nil {
		t.Fatalf("the replaced module: %v", err)
	}
	if i, _ := info(t, e, "basic"); !i.Running || i.Size != len(tiny(false, 0)) || i.Language != "" {
		t.Fatalf("Modules still says the old one: %+v", i)
	}
	os.Remove(filepath.Join(dir, "basic.pb.wasm"))
	_, err := call(t, e, "basic", "run", nil)
	wantCode(t, err, wasm.CodeNotFound)
	if got := e.h.Modules(); len(got) != 0 {
		t.Fatalf("still listed: %+v", got)
	}

	// a call in progress finishes with the module it read, whatever happens to the file
	install(t, dir, "spin", tiny(true, 0))
	done := make(chan error, 1)
	go func() {
		_, err := e.h.Call(context.Background(), "spin", "run", nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	os.Remove(filepath.Join(dir, "spin.pb.wasm"))
	var we *wasm.Error
	if err := <-done; !errors.As(err, &we) || we.Code != wasm.CodeTimeout {
		t.Fatalf("the call in progress: %v", err)
	}
}
