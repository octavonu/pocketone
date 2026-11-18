package wasm

import (
	_ "embed"
	"fmt"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/extkit/js"
)

// Extension is ext/wasm as a PocketOne extension ([extkit.Extension]): the same value works on a standalone app and
// on every instance Pocker runs, each instance with its own modules in its own pb_hooks/.
//
//	set := standard.New()                            // has it: set.WASM, on every app, standalone or an instance
//	set.WASM = wasm.New(wasm.Config{MaxConcurrent: 4}) // reconfigure it
//
// An instance's `extensions.wasm` settings apply over the Config given to New: `dir`, `maxCallMemoryMB`, `maxConcurrent`,
// `maxModulesMB`, `timeout` ("5s", or nanoseconds), `maxPayload`, and `enabled`, which the host checks.
type Extension struct {
	cfg Config
}

var (
	_ extkit.Extension = (*Extension)(nil)
	_ extkit.JSBinder  = (*Extension)(nil)
)

// New returns the extension. cfg holds the defaults for every app it's registered on.
func New(cfg Config) *Extension { return &Extension{cfg: cfg} }

// Name implements [extkit.Extension].
func (x *Extension) Name() string { return Name }

// settings is the shape of the extension's settings layers: [Config]'s fields, with `timeout` read as "5s" (or
// nanoseconds).
type settings struct {
	Dir             *string          `json:"dir"`
	MaxCallMemoryMB *int             `json:"maxCallMemoryMB"`
	MaxConcurrent   *int             `json:"maxConcurrent"`
	MaxModulesMB    *int             `json:"maxModulesMB"`
	Timeout         *extkit.Duration `json:"timeout"`
	MaxPayload      *int             `json:"maxPayload"`
}

// over is c with what s sets.
func (s settings) over(c Config) Config {
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	if s.Dir != nil {
		c.Dir = *s.Dir
	}
	set(&c.MaxCallMemoryMB, s.MaxCallMemoryMB)
	set(&c.MaxConcurrent, s.MaxConcurrent)
	set(&c.MaxModulesMB, s.MaxModulesMB)
	if s.Timeout != nil {
		c.Timeout = time.Duration(*s.Timeout)
	}
	set(&c.MaxPayload, s.MaxPayload)
	return c
}

// Contracts implements [extkit.Declarer]: wasm_list_modules goes to the app's MCP server, when it has one.
func (x *Extension) Contracts() extkit.Contracts {
	return extkit.Contracts{WorksWith: extkit.IDs("mcp/v1")}
}

// Register implements [extkit.Extension]: the app's settings layers apply over the Config given to [New].
func (x *Extension) Register(app core.App, layers extkit.Config) error {
	var s settings
	if err := layers.Decode(&s); err != nil {
		return fmt.Errorf("wasm: %w", err)
	}
	_, err := Register(app, s.over(x.cfg))
	return err
}

// BindJS implements [extkit.JSBinder]: $wasm in the app's JS hooks. It only runs the app's own modules, which
// reach nothing but their input, so it's safe for an untrusted tenant, even in an isolate (Pocker).
//
// The app's Host is looked up when $wasm is called, not here, so it doesn't matter whether the JS engine starts
// before or after Register.
func (x *Extension) BindJS(app core.App, rt js.Runtime) {
	BindJS(app, rt)
}

//go:embed wasm.d.ts
var jsTypes string

// TypeScript implements jsvm's Typer: $wasm's declarations, added to the app's pb_data/types.d.ts.
func (x *Extension) TypeScript() string { return jsTypes }
