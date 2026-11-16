package wasm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/wasm"
)

// tiny is a WASI program made by hand, a few dozen bytes: a _start that returns at once, or, with spin, one that
// loops until the call's time limit. pad bytes in a custom section make the module as big as a test needs.
func tiny(spin bool, pad int) []byte {
	body := []byte{0x00, 0x0b} // no locals; end
	if spin {
		body = []byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b} // no locals; loop: br 0; end; end
	}
	sec := func(id byte, content []byte) []byte {
		return append(append([]byte{id}, uleb(len(content))...), content...)
	}
	b := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	b = append(b, sec(1, []byte{0x01, 0x60, 0x00, 0x00})...)                              // one type: () -> ()
	b = append(b, sec(3, []byte{0x01, 0x00})...)                                          // one function of it
	b = append(b, sec(7, append(append([]byte{0x01, 0x06}, "_start"...), 0x00, 0x00))...) // exported as _start
	b = append(b, sec(10, append([]byte{0x01}, append(uleb(len(body)), body...)...))...)  // its body
	if pad > 0 {
		b = append(b, sec(0, append([]byte{0x03, 'p', 'a', 'd'}, make([]byte, pad)...))...)
	}
	return b
}

// uleb is n in unsigned LEB128, as wasm writes sizes.
func uleb(n int) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		if n >>= 7; n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// TestLimits: Plugins and $wasm.plugins() give each module's size, $wasm.limits() what every call runs within.
func TestLimits(t *testing.T) {
	small, big := tiny(false, 0), tiny(false, 1<<20)
	e := newApp(t, wasm.Config{}, func(_ core.App, dir string) {
		install(t, dir, "small", small, nil)
		install(t, dir, "big", big, nil)
	})
	for name, want := range map[string]int{"small": len(small), "big": len(big)} {
		if i, _ := info(t, e, name); !i.Running || i.Size != want {
			t.Errorf("%s: %+v, want size %d", name, i, want)
		}
	}

	got, err := jsEval(t, e.app, `const l = $wasm.limits(), b = $wasm.plugins().find((p) => p.name === "big");
		JSON.stringify([l.maxConcurrent, l.maxModulesMB, l.timeoutMs, l.maxCallMemoryMB, l.maxPayload, b.size > 1 << 20])`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `[8,10,5000,32,4194304,true]` {
		t.Fatalf("limits and size from JS: %s", got)
	}
}

// TestMaxModulesMB: the modules of the calls running at once add up to at most MaxModulesMB, so fewer calls of a
// bigger module run at once, and a busy error gives the numbers; small calls go in while a big one waits for room.
func TestMaxModulesMB(t *testing.T) {
	e := newApp(t, wasm.Config{MaxModulesMB: 4, Timeout: time.Second}, func(_ core.App, dir string) {
		install(t, dir, "small", tiny(false, 0), nil)
		install(t, dir, "big", tiny(false, 2<<20), nil)  // 2 MB
		install(t, dir, "spin", tiny(true, 5<<19), nil)  // 2.5 MB, runs until the time limit
		install(t, dir, "huge", tiny(false, 5<<20), nil) // 5 MB: over MaxModulesMB, so it doesn't load
	})
	if i, _ := info(t, e, "huge"); i.Running || !strings.Contains(i.Error, "module of 5.0 MB, over MaxModulesMB (4 MB)") {
		t.Fatalf("huge: %+v", i)
	}
	spinning := make(chan error, 1)
	go func() {
		_, err := e.h.Call(context.Background(), "spin", "run", nil)
		spinning <- err
	}()
	time.Sleep(200 * time.Millisecond) // it holds 2.5 of the 4 MB

	// a big one doesn't fit (2.5 + 2 > 4), so it waits
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := e.h.Call(ctx, "big", "run", nil)
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond)

	// small ones fit, and go in ahead of the big one waiting
	start := time.Now()
	small := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := e.h.Call(context.Background(), "small", "run", nil)
			small <- err
		}()
	}
	for range 4 {
		if err := <-small; err != nil {
			t.Fatalf("small: %v", err)
		}
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("the small calls took %s: they waited behind the big one", d)
	}

	// the big one gives up as busy, with the numbers
	var we *wasm.Error
	if err := <-waiting; !errors.As(err, &we) || we.Code != wasm.CodeBusy || !we.Retryable ||
		!strings.Contains(we.Message, "1 of 8 calls were running, with 2.5 MB of 4 MB") ||
		!strings.Contains(we.Message, "module is 2.0 MB") {
		t.Fatalf("big, waiting: %v", err)
	}
	if err := <-spinning; !errors.As(err, &we) || we.Code != wasm.CodeTimeout {
		t.Fatalf("spin: %v", err)
	}
	// with the spinning call over, there's room
	if _, err := e.h.Call(context.Background(), "big", "run", nil); err != nil {
		t.Fatalf("big, after: %v", err)
	}
}
