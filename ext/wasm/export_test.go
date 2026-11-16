package wasm

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
)

// For the tests only (this file isn't part of the package outside go test): plugins are otherwise called only from
// pb_hooks.

// The suite calls the same few plugins thousands of times; compiling each from scratch (~1.5 s for a Go plugin,
// several times that under -race) would take hours. Outside the tests nothing is cached; Cold turns the cache off
// for a test that checks that path.
func init() { compileCache = wazero.NewCompilationCache() }

// Cold turns the test cache off until t ends: every call compiles, as in production.
func Cold(t *testing.T) {
	compileCache = nil
	t.Cleanup(func() { compileCache = wazero.NewCompilationCache() })
}

// Call runs a plugin function directly.
func (h *Host) Call(ctx context.Context, name, function string, input []byte) ([]byte, error) {
	return h.run(ctx, name, function, input)
}

// Config is the settings the Host runs with.
func (h *Host) Config() Config { return h.cfg }

// ProducersLanguage is the language a "producers" custom section names.
var ProducersLanguage = producersLanguage
