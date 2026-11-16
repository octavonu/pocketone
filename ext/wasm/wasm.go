// Package wasm runs untrusted plugins as WebAssembly modules inside a PocketBase app. A plugin is a pure function:
// input in, output out, or an error. It never touches the database, the files or the network; the app's pb_hooks give
// it what it needs and decide what to do with the result. pb_hooks ($wasm) are the only way to call a plugin: no
// Go API, no endpoint, no form.
//
//	onRecordCreate((e) => {
//	  const out = $wasm.run("summary", { title: e.record.get("title"), body: e.record.get("body") })
//	  e.record.set("summary", out.text)
//	  e.next()
//	}, "posts")
//
// A plugin is a plain WASI program (wasip1: Go, Rust, Zig, C, ...): it reads its input from stdin, writes its output
// to stdout, and exits non-zero when it fails, the way a command-line tool would; the function asked for is its
// first argument. It runs in wazero, in the PocketBase process, with no files, network, environment or processes, a
// memory cap and a time limit, in a fresh instance for every call. The plugin has no way out: whatever it needs
// comes in as input, and the network stays with the app's code ($http.send in pb_hooks).
//
// A plugin is a file in the app's plugin folder, pb_wasm/ beside pb_data/ (and pb_hooks/): <name>.wasm, the module,
// and optionally <name>.json, its manifest. It ships with the rest of the app's code (git, `pocketone deploy`,
// SFTP). Like `node script.js`, every call reads the plugin's files as they are then: a plugin added, replaced or
// removed takes effect on the next call, with no restart, and nothing stays in memory between calls. To turn a
// plugin off, remove or rename its .wasm. Copy a new version in under another name and rename it over the old one,
// so no call reads it half written (files starting with "." are left alone).
//
// A single app:
//
//	wasm.MustRegister(app, wasm.Config{})
//
// or [New] as an [extkit.Extension], for examples/pocketone and pocker (see [Extension]).
package wasm

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"ganigeorgiev.com/pocketbase/pocketbase/tools/hook"
)

// Name is the extension's name: its key under an instance's `extensions:` block.
const Name = "wasm"

const storeKey = "ext/wasm"

// Config is the extension's settings. Zero values mean the defaults.
type Config struct {
	// Dir is the plugin folder (default pb_wasm beside the app's data dir).
	Dir string

	// MaxCallMemoryMB caps what each call's program may allocate (default 32): its heap, stack and input. Every call
	// has its own, not shared: 8 calls at once may take 8 times that. A call that needs more crashes alone, and the
	// app goes on.
	MaxCallMemoryMB int

	// MaxConcurrent is how many plugin calls run at once in the app (default 8); more wait, up to Timeout.
	MaxConcurrent int

	// MaxModulesMB is how many MB of plugin modules run at once in the app (default 10): the .wasm sizes of the
	// calls running, added up. Compiling takes time and memory in step with a module's size, so with 10, two calls of
	// a Go plugin (~4.4 MB) run at once, and MaxConcurrent calls of a Rust or Zig one (~0.1 MB). More wait, up to
	// Timeout. It's also the largest module that runs.
	MaxModulesMB int

	// Timeout is the time limit of each plugin call (default 5s).
	Timeout time.Duration

	// MaxPayload is the largest input or output of a call, in bytes (default 4 MB).
	MaxPayload int
}

func (c Config) withDefaults() Config {
	orDefault(&c.MaxCallMemoryMB, 32)
	orDefault(&c.MaxConcurrent, 8)
	orDefault(&c.MaxModulesMB, 10)
	orDefault(&c.Timeout, 5*time.Second)
	orDefault(&c.MaxPayload, 4<<20)
	return c
}

// orDefault sets *v to def when it's zero or negative.
func orDefault[T int | time.Duration](v *T, def T) {
	if *v <= 0 {
		*v = def
	}
}

// MustRegister is [Register] that panics.
func MustRegister(app core.App, cfg Config) *Host {
	h, err := Register(app, cfg)
	if err != nil {
		panic(err)
	}
	return h
}

// Host runs one app's plugins.
type Host struct {
	app core.App
	cfg Config

	mu      sync.RWMutex
	serving bool
	checked map[string]checked // what checking each plugin's files found, while they stay the same

	slotMu  sync.Mutex
	running int           // calls running, at most MaxConcurrent
	bytes   int           // their modules' sizes added up, at most MaxModulesMB
	freed   chan struct{} // closed (and replaced) whenever a call ends, for the calls waiting for room
}

// Info is how one plugin in the folder is doing ($wasm.plugins() in pb_hooks).
type Info struct {
	Name        string   `json:"name"`
	Running     bool     `json:"running"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"` // as its manifest says
	Functions   []string `json:"functions,omitempty"`
	// Language is what it's written in: what the module itself says (Go always does, Rust unless stripped), else its
	// manifest's language.
	Language   string    `json:"language,omitempty"`
	Repository string    `json:"repository,omitempty"` // where its source is, as its manifest says
	Examples   []Example `json:"examples,omitempty"`
	Size       int       `json:"size,omitempty"`  // its module's size in bytes: what a call of it takes of MaxModulesMB
	Error      string    `json:"error,omitempty"` // why it can't run
}

// Limits is what every plugin call in the app runs within ($wasm.limits() in pb_hooks).
type Limits struct {
	MaxCallMemoryMB int   `json:"maxCallMemoryMB"`
	TimeoutMs       int64 `json:"timeoutMs"`
	MaxConcurrent   int   `json:"maxConcurrent"`
	MaxModulesMB    int   `json:"maxModulesMB"`
	MaxPayload      int   `json:"maxPayload"`
}

// Limits is what every plugin call in the app runs within.
func (h *Host) Limits() Limits {
	c := h.cfg
	return Limits{MaxCallMemoryMB: c.MaxCallMemoryMB, TimeoutMs: c.Timeout.Milliseconds(), MaxConcurrent: c.MaxConcurrent,
		MaxModulesMB: c.MaxModulesMB, MaxPayload: c.MaxPayload}
}

// For returns the app's Host, or nil.
func For(app core.App) *Host {
	h, _ := app.Store().Get(storeKey).(*Host)
	return h
}

// Register sets ext/wasm up on app. Plugins run only while the app serves; each call reads its plugin's files from the
// plugin folder. Nothing watches the folder, and nothing runs between calls.
func Register(app core.App, cfg Config) (*Host, error) {
	cfg = cfg.withDefaults()
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(app.DataDir(), "..", "pb_wasm")
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	cfg.Dir = dir
	h := &Host{app: app, cfg: cfg, checked: map[string]checked{}, freed: make(chan struct{})}
	app.Store().Set(storeKey, h)

	// plugins only run while serving, so other commands (migrate, superuser, ...) never compile them
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: "ext-wasm-serve",
		Func: func(e *core.ServeEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			h.mu.Lock()
			h.serving = true
			h.mu.Unlock()
			h.makeDir()
			h.Plugins() // checks every plugin once, so the log says at start which ones can't run
			return nil
		},
		Priority: -1000,
	})
	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		h.mu.Lock()
		h.serving = false
		h.checked = map[string]checked{}
		h.mu.Unlock()
		return e.Next()
	})
	return h, nil
}

// Dir is the plugin folder.
func (h *Host) Dir() string { return h.cfg.Dir }

// Plugins is every plugin in the folder now, by name, and whether it can run (or why not).
func (h *Host) Plugins() []Info {
	names, err := h.list()
	if err != nil {
		h.app.Logger().Warn("wasm: can't read the plugin folder", "dir", h.cfg.Dir, "error", err)
		return nil
	}
	infos := make([]Info, 0, len(names))
	for name := range names {
		infos = append(infos, h.check(name))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos
}

// checked is what checking a plugin's files found, kept while they stay the same (the same sizes and modification
// times), so listing the folder doesn't check every module again. It holds no code: a call reads the files itself.
type checked struct {
	stamp string
	info  Info
}

// check is what the plugin's files say now: its manifest, and whether its module is one that can run (wazero's
// interpreter validates it without making machine code: ~180 ms for a Go plugin, against ~1.5 s to compile it).
func (h *Host) check(name string) Info {
	stamp := h.stamp(name)
	h.mu.RLock()
	c, ok := h.checked[name]
	h.mu.RUnlock()
	if ok && c.stamp == stamp {
		return c.info
	}
	info := Info{Name: name}
	if p, err := h.checkFiles(name); err != nil {
		info.Error = err.Error()
		h.app.Logger().Error("wasm: plugin can't run", "plugin", name, "error", err)
	} else {
		info = Info{Name: name, Running: true, Description: p.manifest.Description, Version: p.manifest.Version,
			Functions: p.manifest.functions(), Language: cmp.Or(p.language, p.manifest.Language),
			Repository: p.manifest.Repository, Examples: p.manifest.Examples, Size: p.size}
		h.app.Logger().Info("wasm: plugin checked", "plugin", name, "functions", p.manifest.functions())
	}
	h.mu.Lock()
	h.checked[name] = checked{stamp: stamp, info: info}
	h.mu.Unlock()
	return info
}

// stamp is the sizes and modification times of the plugin's files, to tell when they change.
func (h *Host) stamp(name string) string {
	var b strings.Builder
	for _, ext := range []string{".wasm", ".json"} {
		if fi, err := os.Stat(filepath.Join(h.cfg.Dir, name+ext)); err == nil {
			fmt.Fprintf(&b, "%d@%d ", fi.Size(), fi.ModTime().UnixNano())
		} else {
			b.WriteString("- ")
		}
	}
	return b.String()
}

// makeDir creates the plugin folder when it isn't there, so whoever runs the app sees where plugins go. One that
// can't be made (a read-only disk) is only logged: the app serves with no plugins.
func (h *Host) makeDir() {
	if fi, err := os.Stat(h.cfg.Dir); err == nil && fi.IsDir() {
		return
	}
	if err := os.MkdirAll(h.cfg.Dir, 0o755); err != nil {
		h.app.Logger().Warn("wasm: can't create the plugin folder", "dir", h.cfg.Dir, "error", err)
		return
	}
	h.app.Logger().Info("wasm: created the plugin folder; put plugins there", "dir", h.cfg.Dir)
}

// list is the names of the plugins in the folder: its <name>.wasm files. Others (hidden ones, an upload's temporary
// file, a .json without its module) are left alone; a .wasm whose name can't be called is reported.
func (h *Host) list() (map[string]bool, error) {
	files, err := os.ReadDir(h.cfg.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, f := range files {
		name, ok := strings.CutSuffix(f.Name(), ".wasm")
		if !ok || f.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !validName.MatchString(name) {
			h.app.Logger().Warn("wasm: not a plugin name (letters, digits, - and _)", "file", f.Name())
			continue
		}
		names[name] = true
	}
	return names, nil
}

// checkFiles reads the plugin's files and checks them, for Plugins: its manifest, and its module validated.
func (h *Host) checkFiles(name string) (*plugin, error) {
	p, err := h.open(name)
	if err != nil {
		return nil, err
	}
	code, err := p.read()
	if err != nil {
		return nil, err
	}
	if p.language, err = validate(code, h.cfg.MaxCallMemoryMB); err != nil {
		return nil, fmt.Errorf("module: %w", err)
	}
	return p, nil
}

// open is the plugin called name as its files are now: its manifest read and parsed, and its module's size (the
// module itself is read later, by read, once a call has room). The error says why it can't run; fs.ErrNotExist when
// there's no <name>.wasm.
func (h *Host) open(name string) (*plugin, error) {
	if !validName.MatchString(name) {
		return nil, fs.ErrNotExist
	}
	fi, err := os.Stat(filepath.Join(h.cfg.Dir, name+".wasm"))
	if err == nil && fi.IsDir() {
		err = fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if max := int64(h.cfg.MaxModulesMB) << 20; fi.Size() > max {
		return nil, fmt.Errorf("module of %s, over MaxModulesMB (%d MB)", size(int(fi.Size())), h.cfg.MaxModulesMB)
	}
	raw, err := os.ReadFile(filepath.Join(h.cfg.Dir, name+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(raw)
	if err != nil {
		return nil, fmt.Errorf("%s.json: %w", name, err)
	}
	return &plugin{host: h, name: name, manifest: m, size: int(fi.Size())}, nil
}

// read reads the plugin's module, which may have changed since open: still within MaxModulesMB.
func (p *plugin) read() ([]byte, error) {
	code, err := os.ReadFile(filepath.Join(p.host.cfg.Dir, p.name+".wasm"))
	if err != nil {
		return nil, err
	}
	if max := p.host.cfg.MaxModulesMB << 20; len(code) > max {
		return nil, fmt.Errorf("module of %s, over MaxModulesMB (%d MB)", size(len(code)), p.host.cfg.MaxModulesMB)
	}
	return code, nil
}
