package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/sys"
)

// Error codes. A plugin's own errors carry the code it chose (see Error).
const (
	CodeNotFound       = "not_found"        // no plugin by that name in the plugin folder
	CodeNotLoaded      = "not_loaded"       // in the folder, but it can't run: the app isn't serving, or its file is wrong (the message says why)
	CodeNoSuchFunction = "no_such_function" // the plugin has no such function: the code a plugin gives for one it doesn't know (by convention)
	CodeTimeout        = "timeout"          // over the time limit
	CodeCrashed        = "crashed"          // the plugin trapped, panicked or ran out of memory
	CodeTooLarge       = "too_large"        // input or output over MaxPayload
	CodeBusy           = "busy"             // the calls running left no room (MaxConcurrent, MaxModulesMB) for longer than the wait allowed
	CodePluginError    = "plugin_error"     // the plugin exited non-zero with no code of its own
)

// Error is a failed plugin call; in pb_hooks, the WasmError a $wasm call throws. A plugin fails by exiting
// non-zero: if what it wrote to stderr is a JSON object {"code", "message", "retryable"}, it's passed on as is;
// any other text becomes Message, with CodePluginError.
type Error struct {
	Plugin    string `json:"plugin"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("wasm plugin %s: %s: %s", e.Plugin, e.Code, e.Message)
}

// plugin is a plugin as its file was when a call (or Plugins) found it: its module's size. A call reads the module
// itself once it has room, and finishes with what it read, whatever the file does meanwhile.
type plugin struct {
	host *Host
	name string
	size int // the module's size in bytes, when it was opened
}

// run runs the plugin's function with input (its stdin) and returns its output (its stdout). Every call compiles the
// plugin from its file, as it is then, and runs it in a runtime of its own, so nothing from one call reaches another,
// and nothing stays in memory after. Up to MaxConcurrent calls run at once; more wait. Errors are *Error.
//
// It's unexported on purpose: plugins are called from the app's pb_hooks ($wasm, see BindJS) and from nowhere else,
// no Go API, no endpoint.
func (h *Host) run(ctx context.Context, name, function string, input []byte) ([]byte, error) {
	r, err := h.runReport(ctx, name, function, input)
	return r.stdout, err
}

// report is what one call left, for $wasm.inspect: its output, what it wrote to stderr, and how long compiling and
// running took. On an error, whatever the call got to.
type report struct {
	stdout           []byte
	stderr           string // up to maxReportedStderr
	compile, running time.Duration
}

const maxReportedStderr = 64 << 10

// runReport is run, with the call's report.
func (h *Host) runReport(ctx context.Context, name, function string, input []byte) (report, error) {
	p, err := h.find(name)
	if err != nil {
		return report{}, err
	}
	if len(input) > h.cfg.MaxPayload {
		return report{}, &Error{Plugin: name, Code: CodeTooLarge, Message: fmt.Sprintf("input over %d bytes", h.cfg.MaxPayload)}
	}

	// at most MaxConcurrent calls at once, their modules at most MaxModulesMB together (compiling included), so memory
	// stays bounded: wait for room, up to Timeout
	wait := time.NewTimer(h.cfg.Timeout)
	defer wait.Stop()
	release, err := h.take(ctx, p.size, wait.C)
	if err != nil {
		return report{}, h.busy(p, err)
	}
	defer release()
	return p.call(ctx, function, input)
}

// errWaited is take giving up because the wait ran out.
var errWaited = errors.New("waited too long")

// take waits for room for a call of a module of size bytes: fewer than MaxConcurrent calls running, and size left
// in MaxModulesMB. A call goes in whenever there's room for it, so a small one doesn't queue behind a big one that's
// waiting. It returns the function that gives the room back, or errWaited or ctx's error when wait or ctx ends first.
func (h *Host) take(ctx context.Context, size int, wait <-chan time.Time) (release func(), err error) {
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
		case <-wait:
			return nil, errWaited
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// busy is the error of a call that found no room, with the numbers: the module's size against what was running.
func (h *Host) busy(p *plugin, err error) *Error {
	h.slotMu.Lock()
	running, bytes := h.running, h.bytes
	h.slotMu.Unlock()
	why := "no room for it"
	if !errors.Is(err, errWaited) {
		why = "the caller gave up waiting for room"
	}
	return &Error{Plugin: p.name, Code: CodeBusy, Retryable: true, Message: fmt.Sprintf(
		"%s: %d of %d calls were running, with %s of %d MB of modules (MaxModulesMB); this plugin's module is %s",
		why, running, h.cfg.MaxConcurrent, size(bytes), h.cfg.MaxModulesMB, size(p.size))}
}

// size is n bytes for people: "95 KB", "4.3 MB".
func size(n int) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// find is the plugin called name as its file is now, or an *Error that says why it can't be called.
func (h *Host) find(name string) (*plugin, error) {
	h.mu.RLock()
	serving := h.serving
	h.mu.RUnlock()
	if !serving {
		return nil, &Error{Plugin: name, Code: CodeNotLoaded, Message: "plugins run only while the app serves"}
	}
	p, err := h.open(name)
	if err != nil {
		return nil, h.cantRun(name, err)
	}
	return p, nil
}

// cantRun is the *Error of a plugin whose file can't run: not there, or what's wrong with them.
func (h *Host) cantRun(name string, err error) *Error {
	if errors.Is(err, fs.ErrNotExist) {
		return &Error{Plugin: name, Code: CodeNotFound, Message: fmt.Sprintf("no such plugin (%s%s in the plugin folder)", name, fileSuffix)}
	}
	return &Error{Plugin: name, Code: CodeNotLoaded, Message: "the plugin didn't load: " + err.Error()}
}

// call reads the plugin's module, compiles it and runs function once. Compiling validates the module (a broken or
// half-written one doesn't run); Timeout limits the run, and compiling comes before it, bounded by the module's size
// (at most MaxModulesMB), and can't be interrupted.
func (p *plugin) call(ctx context.Context, function string, input []byte) (report, error) {
	h := p.host
	start := time.Now()
	code, err := p.read()
	if err != nil {
		return report{}, h.cantRun(p.name, err)
	}
	rt, err := newRuntime(ctx, h.cfg.MaxCallMemoryMB)
	if err != nil {
		return report{}, &Error{Plugin: p.name, Code: CodeCrashed, Message: "runtime: " + err.Error()}
	}
	defer rt.Close(context.Background()) // drops the compiled module with it
	mod, err := rt.CompileModule(ctx, code)
	if err == nil {
		err = checkModule(mod)
	}
	if err != nil {
		h.app.Logger().Warn("wasm: plugin can't run", "plugin", p.name, "error", err)
		return report{compile: time.Since(start)}, h.cantRun(p.name, fmt.Errorf("module: %w", err))
	}
	compiled := time.Since(start)

	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()
	r := runModule(ctx, rt, mod, p.name, function, input, h.cfg.MaxPayload)
	rep := report{stderr: r.stderr[:min(len(r.stderr), maxReportedStderr)], compile: compiled,
		running: time.Since(start) - compiled}
	attrs := []any{"plugin", p.name, "function", function, "compile", compiled.String(), "run", rep.running.String()}
	if s := strings.TrimSpace(r.stderr); s != "" {
		attrs = append(attrs, "stderr", s[:min(len(s), maxLoggedStderr)])
	}
	if e := p.explain(ctx, r); e != nil {
		if e.Code == CodeCrashed && outOfMemory(e.Message+"\n"+r.stderr) {
			e.Message += fmt.Sprintf(" (each call may allocate up to %d MB: MaxCallMemoryMB)", h.cfg.MaxCallMemoryMB)
		}
		h.app.Logger().Warn("wasm: plugin call failed", append(attrs, "code", e.Code, "error", e.Message)...)
		return rep, e
	}
	if len(attrs) > 8 {
		h.app.Logger().Info("wasm: plugin call", attrs...) // what it wrote to stderr: its log
	} else {
		h.app.Logger().Debug("wasm: plugin call", attrs...)
	}
	rep.stdout = r.stdout
	return rep, nil
}

const (
	maxLoggedStderr = 4 << 10
	maxMessage      = 1 << 10
)

// explain turns a failed run into an *Error; nil if it worked.
func (p *plugin) explain(ctx context.Context, r result) *Error {
	e := &Error{Plugin: p.name}
	stderr := strings.TrimSpace(r.stderr)
	var exit *sys.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		e.Code, e.Message, e.Retryable = CodeTimeout, fmt.Sprintf("over the time limit (%s)", p.host.cfg.Timeout), true
	case ctx.Err() != nil:
		e.Code, e.Message, e.Retryable = CodeTimeout, "the call was cancelled", true
	case r.tooLarge:
		e.Code, e.Message = CodeTooLarge, fmt.Sprintf("output over %d bytes", p.host.cfg.MaxPayload)
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
		case goCrash(stderr) != "": // a Go plugin's panic or fatal error: exit code 2, the reason on stderr
			e.Code, e.Message = CodeCrashed, goCrash(stderr)
		case stderr != "":
			e.Code, e.Message = CodePluginError, stderr[:min(len(stderr), maxMessage)]
		default:
			e.Code, e.Message = CodePluginError, fmt.Sprintf("exited with code %d", r.exitCode)
		}
	default:
		// a trap: the first line; the wasm stack trace after it goes to the log, not to the caller
		msg, trace, _ := strings.Cut(r.err.Error(), "\nwasm stack trace:")
		e.Code, e.Message = CodeCrashed, strings.TrimSuffix(msg, " (recovered by wazero)")
		if trace != "" {
			p.host.app.Logger().Debug("wasm: plugin stack trace", "plugin", p.name, "trace", trace)
		}
		if stderr != "" { // a Rust or Zig panic says why on stderr, then traps
			line, _, _ := strings.Cut(stderr, "\n")
			for _, l := range strings.Split(stderr, "\n") { // out of memory: that line, after whatever it logged
				if outOfMemory(l) {
					line = l
					break
				}
			}
			e.Message = line[:min(len(line), maxMessage)] + " (" + e.Message + ")"
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
			return line[:min(len(line), maxMessage)]
		}
	}
	return ""
}

// outOfMemory is whether a crash says the program ran out of memory: Go's "fatal error: out of memory", Rust's
// "memory allocation of N bytes failed".
func outOfMemory(s string) bool {
	return strings.Contains(s, "out of memory") || strings.Contains(s, "memory allocation of")
}
