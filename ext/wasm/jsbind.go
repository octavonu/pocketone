package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/extkit/js"
)

// BindJS exposes the app's plugins to its JS hooks as the $wasm global, on one of its JS runtimes. [Extension] binds
// it for you; on an app without extkit, add it to the jsvm OnInit:
//
//	jsvm.MustRegister(app, jsvm.Config{OnInit: func(rt js.Runtime) { wasm.BindJS(app, rt) }})
//
// In a pb_hooks file:
//
//	onRecordCreate((e) => {
//	  const out = $wasm.run("summary", { title: e.record.get("title"), body: e.record.get("body") })
//	  e.record.set("summary", out.text)
//	  e.next()
//	}, "posts")
//
//	$wasm.run(name, input)            -> output: calls the plugin's "run"
//	$wasm.call(name, function, input) -> output: calls one of its functions (the first argument the plugin gets)
//	$wasm.plugins()                   -> every plugin in the folder, with what it says for --describe: {name,
//	                                     running, description, functions, language, repository, examples, size, error}
//	$wasm.limits()                    -> what every call runs within: {maxCallMemoryMB, timeoutMs, maxConcurrent,
//	                                     maxModulesMB, maxPayload}
//	$wasm.inspect(name, function, input) -> {output, stderr, error, compileMs, runMs}: call, for trying a plugin
//	                                     out; it never throws for the call, error is {code, message, retryable}
//	                                     or null, and stderr is what the plugin wrote there (its log)
//
//	await $wasm.run.async(name, input), $wasm.call.async(...), $wasm.inspect.async(...): the same, run off the
//	                                     app's JS VM, so the app serves other requests while the plugin runs
//
// The input is the plugin's stdin: a string or bytes (an ArrayBuffer, a typed array, a []byte from Go) as they are,
// nothing for undefined or null, anything else as JSON. The output is the plugin's stdout: JSON parsed, or a string
// if it isn't JSON, null if empty. A failed call throws an Error named WasmError, with code, message, retryable and plugin (see
// the Code* constants and Error): catch it to decide what the hook does next.
//
// The JS engine runs one VM per app. A sync call holds it for as long as the plugin runs, up to the Timeout in
// Config, so the app's other hooks and requests wait: fine for a plugin that answers in milliseconds (Rust, Zig).
// For a slow one (a Go plugin compiles for over a second), or many calls at once, use .async in an async hook.
func BindJS(app core.App, rt js.Runtime) {
	get := func() (*Host, error) {
		if h := For(app); h != nil {
			return h, nil
		}
		return nil, errors.New("wasm isn't registered on this app")
	}
	// The Go side answers in JSON text, which the JS side below parses, so a hook gets plain JS data (objects,
	// arrays, strings) and a failed call throws a real Error. Plain Go funcs, so the engine gives each an .async twin.
	call := func(report bool) func(name, function string, in any, bytes bool) (string, error) {
		return func(name, function string, in any, bytes bool) (string, error) {
			h, err := get()
			if err != nil {
				return "", err
			}
			input, err := stdin(name, in, bytes)
			if err != nil {
				return "", err
			}
			r, err := h.runReport(context.Background(), name, function, input)
			res := map[string]any{"output": string(r.stdout)}
			var e *Error
			switch {
			case errors.As(err, &e):
				res["error"] = map[string]any{"code": e.Code, "message": e.Message, "retryable": e.Retryable, "plugin": e.Plugin}
			case err != nil:
				return "", err
			}
			if report {
				res["stderr"] = r.stderr
				res["compileMs"] = float64(r.compile.Microseconds()) / 1000
				res["runMs"] = float64(r.running.Microseconds()) / 1000
			}
			return jsonText(res)
		}
	}
	native := rt.NewObject()
	_ = native.Set("call", call(false))
	_ = native.Set("inspect", call(true))
	_ = native.Set("plugins", func() (string, error) {
		h, err := get()
		if err != nil {
			return "", err
		}
		return jsonText(h.Plugins())
	})
	_ = native.Set("limits", func() (string, error) {
		h, err := get()
		if err != nil {
			return "", err
		}
		return jsonText(h.Limits())
	})
	if err := rt.Set(nativeGlobal, native); err != nil {
		panic(fmt.Errorf("wasm: bind JS: %w", err))
	}
	if _, err := rt.Eval(jsPrelude, "ext/wasm"); err != nil {
		panic(fmt.Errorf("wasm: bind JS: %w", err))
	}
}

const nativeGlobal = "__pocketone_wasm"

// jsPrelude builds $wasm over the Go functions, which it takes off the global scope: it passes the input on (a
// typed array as its bytes), parses the output, and throws a WasmError. run, call and inspect each have an .async
// twin that runs the plugin off the app's JS VM, so the app serves other requests meanwhile.
const jsPrelude = `(() => {
  const native = globalThis.` + nativeGlobal + `;
  delete globalThis.` + nativeGlobal + `;
  const wasmError = (e) => {
    const err = new Error(e.message);
    err.name = "WasmError";
    err.code = e.code;
    err.retryable = e.retryable;
    err.plugin = e.plugin;
    return err;
  };
  const args = (name, fn, v) => {
    if (v instanceof ArrayBuffer) v = new Uint8Array(v);
    if (ArrayBuffer.isView(v)) return [String(name), String(fn), Array.from(new Uint8Array(v.buffer, v.byteOffset, v.byteLength)), true];
    return [String(name), String(fn), v === undefined ? null : v, false];
  };
  const output = (s) => {
    if (s === "") return null;
    try {
      return JSON.parse(s);
    } catch (e) {
      return s;
    }
  };
  const result = (text) => {
    const r = JSON.parse(text);
    if (r.error) throw wasmError(r.error);
    return output(r.output);
  };
  const report = (text) => {
    const r = JSON.parse(text);
    const e = r.error;
    return { output: e ? null : output(r.output), error: e ? { code: e.code, message: e.message, retryable: e.retryable } : null,
      stderr: r.stderr, compileMs: r.compileMs, runMs: r.runMs };
  };
  const both = (sync, async) => Object.defineProperty(sync, "async", { value: async });
  globalThis.$wasm = Object.freeze({
    run: both((name, v) => result(native.call(...args(name, "run", v))),
      async (name, v) => result(await native.call.async(...args(name, "run", v)))),
    call: both((name, fn, v) => result(native.call(...args(name, fn, v))),
      async (name, fn, v) => result(await native.call.async(...args(name, fn, v)))),
    inspect: both((name, fn, v) => report(native.inspect(...args(name, fn, v))),
      async (name, fn, v) => report(await native.inspect.async(...args(name, fn, v)))),
    plugins: () => JSON.parse(native.plugins()),
    limits: () => JSON.parse(native.limits()),
  });
})()`

// stdin is the plugin's stdin from what JS passed: bytes (a typed array, sent as its numbers) and strings as they
// are, nothing for null, anything else (a JS object, a Go value) as JSON.
func stdin(name string, in any, bytes bool) ([]byte, error) {
	switch v := in.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	}
	if nums, ok := in.([]any); ok && bytes {
		b := make([]byte, len(nums))
		for i, n := range nums {
			switch x := n.(type) {
			case float64:
				b[i] = byte(x)
			case int64:
				b[i] = byte(x)
			case int:
				b[i] = byte(x)
			default:
				return nil, fmt.Errorf("$wasm: the input of %s: byte %d isn't a number", name, i)
			}
		}
		return b, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("$wasm: the input of %s can't be sent as JSON: %w", name, err)
	}
	return b, nil
}

// jsonText is v as JSON text, for the JS side to parse.
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
