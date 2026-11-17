package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tetratelabs/wazero/sys"
)

// Error codes. A module's own errors carry the code it chose (see Error).
const (
	CodeNotFound       = "not_found"        // no module by that name in the module folder
	CodeNotLoaded      = "not_loaded"       // in the folder, but it can't run: the app isn't serving, or its file is wrong (the message says why)
	CodeNoSuchFunction = "no_such_function" // the module has no such function: the code a module gives for one it doesn't know (by convention)
	CodeTimeout        = "timeout"          // over the time limit
	CodeCrashed        = "crashed"          // the module trapped, panicked or ran out of memory
	CodeTooLarge       = "too_large"        // input or output over MaxPayload
	CodeBusy           = "busy"             // the calls running left no room (MaxConcurrent, MaxModulesMB) for longer than the wait allowed
	CodeModuleError    = "module_error"     // the module exited non-zero with no code of its own
)

// Error is a failed module call; in pb_hooks, the WasmError a $wasm call throws. A module fails by exiting
// non-zero: if what it wrote to stderr is a JSON object {"code", "message", "retryable"}, it's passed on as is;
// any other text becomes Message, with CodeModuleError.
type Error struct {
	Module    string `json:"module"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("wasm module %s: %s: %s", e.Module, e.Code, e.Message)
}

// module is a <name>.pb.wasm as its file was when a call (or Modules) found it: its size. A call reads the code
// itself once it has room, and finishes with what it read, whatever the file does meanwhile.
type module struct {
	host *Host
	name string
	size int // the module's size in bytes, when it was opened
}

// run runs the module's function with input (its stdin) and returns its output (its stdout). Every call compiles the
// module from its file, as it is then, and runs it in a runtime of its own, so nothing from one call reaches another,
// and nothing stays in memory after. Up to MaxConcurrent calls run at once; more wait. Errors are *Error.
//
// It's unexported on purpose: modules are called from the app's pb_hooks ($wasm, see BindJS) and from nowhere else,
// no Go API, no endpoint.
func (h *Host) run(ctx context.Context, name, function string, input []byte) ([]byte, error) {
	r, err := h.runReport(ctx, name, function, input)
	return r.stdout, err
}

// report is what one call left, for $wasm.inspect.async: its output, what it wrote to stderr, and how long compiling and
// running took. On an error, whatever the call got to.
type report struct {
	stdout           []byte
	stderr           string // up to maxStderr: runModule keeps no more
	compile, running time.Duration
}

// runReport is run, with the call's report.
func (h *Host) runReport(ctx context.Context, name, function string, input []byte) (report, error) {
	m, err := h.find(name)
	if err != nil {
		return report{}, err
	}
	if len(input) > h.cfg.MaxPayload {
		return report{}, &Error{Module: name, Code: CodeTooLarge, Message: fmt.Sprintf("input over %d bytes", h.cfg.MaxPayload)}
	}

	// at most MaxConcurrent calls at once, their modules at most MaxModulesMB together (compiling included), so memory
	// stays bounded: wait for room, up to Timeout
	waitCtx, cancel := context.WithTimeoutCause(ctx, h.cfg.Timeout, errWaited)
	release, err := h.take(waitCtx, m.size)
	cancel() // the wait only: the call itself runs on ctx
	if err != nil {
		return report{}, h.busy(m, err)
	}
	defer release()
	return m.call(ctx, function, input)
}

// errWaited is take giving up because the wait ran out.
var errWaited = errors.New("waited too long")

// take waits for room for a call of a module of size bytes: fewer than MaxConcurrent calls running, and size left
// in MaxModulesMB. A call goes in whenever there's room for it, so a small one doesn't queue behind a big one that's
// waiting. It returns the function that gives the room back, or ctx's cause (errWaited for the wait's own limit)
// when ctx ends first.
func (h *Host) take(ctx context.Context, size int) (release func(), err error) {
	for {
		h.slotMu.Lock()
		if h.running < h.cfg.MaxConcurrent && h.bytes+size <= h.cfg.MaxModulesMB<<20 {
			h.running++
			h.bytes += size
			h.slotMu.Unlock()
			return func() {
				h.slotMu.Lock()
				h.running--
				h.bytes -= size
				close(h.freed) // wakes every call waiting; each checks again
				h.freed = make(chan struct{})
				h.slotMu.Unlock()
			}, nil
		}
		freed := h.freed
		h.slotMu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// busy is the error of a call that found no room, with the numbers: the module's size against what was running.
func (h *Host) busy(m *module, err error) *Error {
	h.slotMu.Lock()
	running, bytes := h.running, h.bytes
	h.slotMu.Unlock()
	why := "no room for it"
	if !errors.Is(err, errWaited) {
		why = "the caller gave up waiting for room"
	}
	return &Error{Module: m.name, Code: CodeBusy, Retryable: true, Message: fmt.Sprintf(
		"%s: %d of %d calls were running, with %s of %d MB of modules (MaxModulesMB); this module is %s",
		why, running, h.cfg.MaxConcurrent, size(bytes), h.cfg.MaxModulesMB, size(m.size))}
}

// size is n bytes for people: "95 KB", "4.3 MB".
func size(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// find is the module called name as its file is now, or an *Error that says why it can't be called.
func (h *Host) find(name string) (*module, error) {
	h.mu.RLock()
	serving := h.serving
	h.mu.RUnlock()
	if !serving {
		return nil, &Error{Module: name, Code: CodeNotLoaded, Message: "modules run only while the app serves"}
	}
	m, err := h.open(name)
	if err != nil {
		return nil, h.cantRun(name, err)
	}
	return m, nil
}

// cantRun is the *Error of a module whose file can't run: not there, or what's wrong with them.
func (h *Host) cantRun(name string, err error) *Error {
	if errors.Is(err, fs.ErrNotExist) {
		return &Error{Module: name, Code: CodeNotFound, Message: fmt.Sprintf("no such module (%s%s in the module folder)", name, fileSuffix)}
	}
	return &Error{Module: name, Code: CodeNotLoaded, Message: "the module didn't load: " + err.Error()}
}

// call reads the module's file, compiles it and runs function once. Compiling validates the module (a broken or
// half-written one doesn't run); Timeout limits the run, and compiling comes before it, bounded by the module's size
// (at most MaxModulesMB), and can't be interrupted.
func (m *module) call(ctx context.Context, function string, input []byte) (report, error) {
	h := m.host
	start := time.Now()
	code, err := m.read()
	if err != nil {
		return report{}, h.cantRun(m.name, err)
	}
	rt, err := newRuntime(ctx, h.cfg.MaxCallMemoryMB)
	if err != nil {
		return report{}, &Error{Module: m.name, Code: CodeCrashed, Message: "runtime: " + err.Error()}
	}
	defer rt.Close(context.Background()) // drops the compiled module with it
	mod, err := rt.CompileModule(ctx, code)
	if err == nil {
		err = checkModule(mod)
	}
	if err != nil {
		h.app.Logger().Warn("wasm: module can't run", "module", m.name, "error", err)
		return report{compile: time.Since(start)}, h.cantRun(m.name, fmt.Errorf("module: %w", err))
	}
	compiled := time.Since(start)

	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	r := runModule(ctx, rt, mod, m.name, function, input, h.cfg.MaxPayload)
	rep := report{stderr: r.stderr, compile: compiled, running: time.Since(start) - compiled}
	attrs := []any{"module", m.name, "function", function, "compile", compiled.String(), "run", rep.running.String()}
	logged := strings.TrimSpace(r.stderr) != "" // what it wrote to stderr is its log
	if logged {
		attrs = append(attrs, "stderr", truncate(strings.TrimSpace(r.stderr), maxLoggedStderr))
	}
	if e := m.explain(ctx, r); e != nil {
		if e.Code == CodeCrashed && outOfMemory(e.Message+"\n"+r.stderr) {
			e.Message += fmt.Sprintf(" (each call may allocate up to %d MB: MaxCallMemoryMB)", h.cfg.MaxCallMemoryMB)
		}
		h.app.Logger().Warn("wasm: module call failed", append(attrs, "code", e.Code, "error", e.Message)...)
		return rep, e
	}
	if logged {
		h.app.Logger().Info("wasm: module call", attrs...)
	} else {
		h.app.Logger().Debug("wasm: module call", attrs...)
	}
	rep.stdout = r.stdout
	return rep, nil
}

const (
	maxLoggedStderr = 4 << 10
	maxMessage      = 1 << 10
)

// explain turns a failed run into an *Error; nil if it worked.
func (m *module) explain(ctx context.Context, r result) *Error {
	e := &Error{Module: m.name}
	stderr := strings.TrimSpace(r.stderr)
	var exit *sys.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		e.Code, e.Message, e.Retryable = CodeTimeout, fmt.Sprintf("over the time limit (%s)", m.host.cfg.Timeout), true
	case ctx.Err() != nil:
		e.Code, e.Message, e.Retryable = CodeTimeout, "the call was cancelled", true
	case r.tooLarge:
		e.Code, e.Message = CodeTooLarge, fmt.Sprintf("output over %d bytes", m.host.cfg.MaxPayload)
	case r.err == nil:
		return nil
	case errors.As(r.err, &exit):
		var structured struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}
		switch {
		case json.Unmarshal([]byte(stderr), &structured) == nil && structured.Code != "":
			e.Code, e.Message, e.Retryable = structured.Code, structured.Message, structured.Retryable
		case goCrash(stderr) != "": // a Go module's panic or fatal error: exit code 2, the reason on stderr
			e.Code, e.Message = CodeCrashed, goCrash(stderr)
		case stderr != "":
			e.Code, e.Message = CodeModuleError, truncate(stderr, maxMessage)
		default:
			e.Code, e.Message = CodeModuleError, fmt.Sprintf("exited with code %d", r.exitCode)
		}
	default:
		// a trap: the first line; the wasm stack trace after it goes to the log, not to the caller
		msg, trace, _ := strings.Cut(r.err.Error(), "\nwasm stack trace:")
		e.Code, e.Message = CodeCrashed, strings.TrimSuffix(msg, " (recovered by wazero)")
		if trace != "" {
			m.host.app.Logger().Debug("wasm: module stack trace", "module", m.name, "trace", trace)
		}
		if stderr != "" { // a Rust or Zig panic says why on stderr, then traps
			line, _, _ := strings.Cut(stderr, "\n")
			for _, l := range strings.Split(stderr, "\n") { // out of memory: that line, after whatever it logged
				if outOfMemory(l) {
					line = l
					break
				}
			}
			e.Message = truncate(line, maxMessage) + " (" + e.Message + ")"
		}
	}
	return e
}

// goCrash is the line where a Go program says why it died ("panic: ...", "fatal error: ..."), or "".
func goCrash(stderr string) string {
	for _, prefix := range []string{"panic: ", "fatal error: "} {
		i := strings.Index(stderr, prefix)
		if i == 0 || (i > 0 && stderr[i-1] == '\n') {
			line, _, _ := strings.Cut(stderr[i:], "\n")
			return truncate(line, maxMessage)
		}
	}
	return ""
}

// truncate is s cut to at most n bytes, never inside a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// outOfMemory is whether a crash says the program ran out of memory: Go's "fatal error: out of memory", Rust's
// "memory allocation of N bytes failed".
func outOfMemory(s string) bool {
	return strings.Contains(s, "out of memory") || strings.Contains(s, "memory allocation of")
}
