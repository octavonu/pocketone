package wasm

import (
	_ "embed"
	"fmt"
	"time"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/extkit/js"
)

// Extension is ext/wasm as a PocketOne extension ([extkit.Extension]): the same value works on a single app and on
// every instance under pocker, each instance with its own plugins in its own pb_hooks/.
//
//	set := standard.New().Add(wasm.New(wasm.Config{})) // a single app, and every pocker instance
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
func (p *Extension) Name() string { return Name }

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

// Register implements [extkit.Extension]: the app's settings layers apply over the Config given to [New].
func (p *Extension) Register(app core.App, layers extkit.Config) error {
	var s settings
	if err := layers.Decode(&s); err != nil {
		return fmt.Errorf("wasm: %w", err)
	}
	_, err := Register(app, s.over(p.cfg))
	return err
}

// BindJS implements [extkit.JSBinder]: $wasm in the app's JS hooks. It only runs the app's own plugins, which
// reach nothing but their input, so it's safe for an untrusted tenant under pocker.
//
// The app's Host is looked up when $wasm is called, not here, so it doesn't matter whether the JS engine starts
// before or after Register.
func (p *Extension) BindJS(app core.App, rt js.Runtime) {
	BindJS(app, rt)
}

//go:embed wasm.d.ts
var jsTypes string

// TypeScript implements jsvm's Typer: $wasm's declarations, added to the app's pb_data/types.d.ts.
func (p *Extension) TypeScript() string { return jsTypes }
