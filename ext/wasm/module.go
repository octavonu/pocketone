package wasm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// A plugin is a WASI program (wasip1), the kind any language with a WASI target builds from a plain main: its input
// on stdin, its output on stdout, and an exit code that says whether it worked. It gets the function asked for as
// its first argument (os.Args[1] in Go), and nothing else: no files, no environment, no network. wasip1 has no
// call that opens a connection, and the host gives it no folder, no variables and no sockets.

// Nothing compiled outlives a call: every call compiles its plugin into a runtime of its own and closes it after, so
// a plugin costs memory only while it runs. Compiling is the slow part (~1.5 s for a Go plugin, ~45 ms for Rust or
// Zig), so plugins on a hot path are better in Rust or Zig.

// compileCache stays nil outside the tests, which set one so the suite doesn't compile the same plugin thousands of
// times (export_test.go).
var compileCache wazero.CompilationCache

// newRuntime builds a runtime for one call: the memory cap, WASI, and a run that stops when its context ends.
func newRuntime(ctx context.Context, maxCallMemoryMB int) (wazero.Runtime, error) {
	cfg := wazero.NewRuntimeConfig().
		WithMemoryLimitPages(uint32(maxCallMemoryMB) * 16). // 64 KiB pages
		WithCloseOnContextDone(true)
	if compileCache != nil {
		cfg = cfg.WithCompilationCache(compileCache)
	}
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx)
		return nil, err
	}
	return rt, nil
}

// validate checks a module when its file loads, so a broken one is refused right away, not on its first call. It
// uses wazero's interpreter, which decodes and validates without making machine code: ~180 ms for a Go plugin,
// against ~1.5 s to compile it.
func validate(code []byte, maxCallMemoryMB int) (language string, err error) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter().
		WithMemoryLimitPages(uint32(maxCallMemoryMB)*16).WithCustomSections(true))
	defer rt.Close(ctx)
	m, err := rt.CompileModule(ctx, code)
	if err != nil {
		return "", err
	}
	if err := checkModule(m); err != nil {
		return "", err
	}
	for _, s := range m.CustomSections() {
		if s.Name() == "producers" {
			return producersLanguage(s.Data()), nil
		}
	}
	return "", nil
}

// producersLanguage is the source language a module's "producers" section names (the tool-conventions format: Go
// writes it, Rust too unless stripped), or "". C is skipped: it's WASI's libc in a Rust or Zig program (Zig names
// only C99), not what the plugin was written in.
func producersLanguage(b []byte) string {
	r := &leb{b: b}
	fields := r.uint()
	for range min(fields, 16) {
		field := r.str()
		values := r.uint()
		for range min(values, 32) {
			name, _ := r.str(), r.str()
			if field == "language" && name != "" && !isC(name) && validLanguage.MatchString(name) && r.ok {
				return name
			}
		}
	}
	return ""
}

func isC(lang string) bool {
	return lang == "C" || (len(lang) > 1 && lang[0] == 'C' && (lang[1] >= '0' && lang[1] <= '9' || lang[1] == '+'))
}

// leb reads the producers section's LEB128 numbers and length-prefixed strings; ok turns false past the end.
type leb struct {
	b  []byte
	ok bool
}

func (r *leb) uint() uint64 {
	var v uint64
	for shift := 0; shift < 64; shift += 7 {
		if len(r.b) == 0 {
			r.b, r.ok = nil, false
			return 0
		}
		c := r.b[0]
		r.b = r.b[1:]
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			r.ok = true
			return v
		}
	}
	r.ok = false
	return 0
}

func (r *leb) str() string {
	n := r.uint()
	if !r.ok || n > uint64(len(r.b)) {
		r.b, r.ok = nil, false
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

// checkModule refuses a module that isn't a plugin: one that imports anything but WASI (built for host functions
// this host doesn't have, such as an Extism PDK's), or a library with no main to run.
func checkModule(m wazero.CompiledModule) error {
	for _, f := range m.ImportedFunctions() {
		mod, name, _ := f.Import()
		if mod == "wasi_snapshot_preview1" {
			continue
		}
		if strings.HasPrefix(mod, "extism:") {
			return fmt.Errorf("imports %s.%s: an Extism plugin; a plugin here is a WASI program that reads stdin and writes stdout", mod, name)
		}
		return fmt.Errorf("imports %s.%s, which this host doesn't provide (plugins get only their input)", mod, name)
	}
	if m.ExportedFunctions()["_start"] == nil {
		return errors.New("not a WASI program (no _start): build it from a main, not as a library (in Go, without -buildmode=c-shared)")
	}
	return nil
}

// result is what a program run left: its output, what it wrote to stderr, how it ended.
type result struct {
	stdout   []byte
	stderr   string
	exitCode uint32
	tooLarge bool  // stdout went over the limit
	err      error // a trap, the context ending, or a non-zero exit (a *sys.ExitError)
}

// runModule runs the module once, in a fresh instance: args [name, function], input on stdin.
func runModule(ctx context.Context, rt wazero.Runtime, m wazero.CompiledModule, name, function string, input []byte, maxOutput int) result {
	out := &capped{max: maxOutput, strict: true}
	errOut := &capped{max: maxStderr}
	mod, err := rt.InstantiateModule(ctx, m, wazero.NewModuleConfig().
		WithName(""). // anonymous: one instance per call
		WithArgs(name, function).
		WithStdin(bytes.NewReader(input)).
		WithStdout(out).WithStderr(errOut).
		WithSysWalltime().WithSysNanotime().
		WithRandSource(rand.Reader))
	if mod != nil {
		mod.Close(context.Background())
	}
	r := result{stdout: out.b.Bytes(), stderr: errOut.b.String(), tooLarge: out.over}
	var exit *sys.ExitError
	if errors.As(err, &exit) {
		r.exitCode = exit.ExitCode()
		if r.exitCode == 0 {
			err = nil
		}
	}
	r.err = err
	return r
}

const maxStderr = 64 << 10

// capped keeps up to max bytes. Past that, a strict one (stdout) fails the write, so the program sees its output
// refused; the other (stderr) drops the rest quietly.
type capped struct {
	b      bytes.Buffer
	max    int
	strict bool
	over   bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.over || c.b.Len()+len(p) > c.max {
		c.over = true
		if room := c.max - c.b.Len(); room > 0 {
			c.b.Write(p[:room])
		}
		if c.strict {
			return 0, errors.New("output over the limit")
		}
		return len(p), nil
	}
	return c.b.Write(p)
}
