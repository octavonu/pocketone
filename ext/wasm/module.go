package wasm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// A plugin is a WASI program (wasip1), the kind any language with a WASI target builds from a plain main: its input
// on stdin, its output on stdout, and an exit code that says whether it worked. It gets the function asked for as
// its first argument (os.Args[1] in Go), and nothing else: no files, no environment, no network. wasip1 has no
// call that opens a connection, and the host gives it no folder, no variables and no sockets.

// Nothing compiled outlives a call: every call compiles its plugin into a runtime of its own and closes it after, so
// a plugin costs memory only while it runs. Compiling is the slow part (~1.7 s for a Go plugin, ~45 ms for Rust or
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

// checkCode checks a module when its file is listed, so a broken one shows right away, not on its first call, and
// asks it for its manifest: the module run once as `<name> --describe`, with nothing on stdin and the limits of a
// call. It uses wazero's interpreter, which decodes and validates without making machine code: ~0.25 s for a Go
// plugin, against ~1.7 s to compile it. err is why the module can't run; describeErr what's wrong with what it
// printed for --describe (the plugin still runs, with the defaults).
func checkCode(code []byte, name string, maxCallMemoryMB int, timeout time.Duration) (language string, m Manifest, describeErr, err error) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter().
		WithMemoryLimitPages(uint32(maxCallMemoryMB)*16).WithCustomSections(true).WithCloseOnContextDone(true))
	defer rt.Close(ctx)
	mod, err := rt.CompileModule(ctx, code)
	if err != nil {
		return "", m, nil, err
	}
	if err := checkModule(mod); err != nil {
		return "", m, nil, err
	}
	for _, s := range mod.CustomSections() {
		if s.Name() == "producers" {
			language = producersLanguage(s.Data())
			break
		}
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return "", m, nil, err
	}
	m, describeErr = describe(ctx, rt, mod, name, timeout)
	return language, m, describeErr, nil
}

// describe runs the module as `<name> --describe` and reads its manifest. A plugin that exits non-zero or prints
// nothing doesn't answer --describe: the zero Manifest, with no error.
func describe(ctx context.Context, rt wazero.Runtime, mod wazero.CompiledModule, name string, timeout time.Duration) (Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := runModule(ctx, rt, mod, name, describeFlag, nil, maxManifest)
	switch {
	case ctx.Err() != nil:
		return Manifest{}, fmt.Errorf("over the time limit (%s)", timeout)
	case r.tooLarge:
		return Manifest{}, fmt.Errorf("over %d KB", maxManifest>>10)
	case r.err != nil || len(bytes.TrimSpace(r.stdout)) == 0:
		return Manifest{}, nil
	}
	m, err := parseManifest(r.stdout)
	if err != nil {
		return Manifest{}, err // nothing of it shown: the defaults
	}
	return m, nil
}

// producersLanguage is the source language a module's "producers" section names (the tool-conventions format: Go
// writes it, Rust too unless stripped), or "". C is skipped: it's WASI's libc in a Rust or Zig program (Zig names
// only C99), not what the plugin was written in.
func producersLanguage(b []byte) string {
	// the section is LEB128 counts (binary.Uvarint) and length-prefixed strings; ok turns false past the end, for good
	ok := true
	num := func() uint64 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			b, ok = nil, false
			return 0
		}
		b = b[n:]
		return v
	}
	str := func() string {
		n := num()
		if !ok || n > uint64(len(b)) {
			b, ok = nil, false
			return ""
		}
		s := string(b[:n])
		b = b[n:]
		return s
	}
	for range min(num(), 16) {
		field := str()
		for range min(num(), 32) {
			name, _ := str(), str()
			if field == "language" && ok && name != "" && !isC(name) && validLanguage.MatchString(name) {
				return name
			}
		}
	}
	return ""
}

func isC(lang string) bool {
	return lang == "C" || (len(lang) > 1 && lang[0] == 'C' && (lang[1] >= '0' && lang[1] <= '9' || lang[1] == '+'))
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
