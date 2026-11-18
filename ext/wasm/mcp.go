package wasm

import (
	"context"
	"encoding/json"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	mcpv1 "github.com/pockethost/pocketone/ext/contracts/mcp/v1"
	"github.com/pockethost/pocketone/ext/extkit"
)

// ToolListModules is the MCP tool that lists the app's modules: an agent connected to the app's /mcp learns which
// modules exist and how to call them, even when their sources aren't in its file tree.
const ToolListModules = "wasm_list_modules"

// mcpInstructions is what the app's MCP server tells agents when they connect.
const mcpInstructions = `WASM modules: pb_hooks/<name>.pb.wasm files are WASI programs the app's JS hooks call: ` +
	"`await $wasm.run.async(name, input)`, or `await $wasm.call.async(name, function, input)` for a function other than " +
	`"run". Calls are async only, so they go in async hooks (or PocketPages that await). A string input is the module's ` +
	`stdin as it is (raw text: EDIFACT, XML, CSV); anything else is sent as JSON, and JSON output comes back parsed. A ` +
	"failed call throws a WasmError with `code`, `message`, `retryable` and `module`. A module reaches nothing but its " +
	`input: the hook reads the data it needs and saves what comes back. ` + ToolListModules + ` lists the modules ` +
	`with what each one says about itself: its functions, and examples with their inputs and outputs.`

// addMCP adds the app's MCP tool and instructions (mcp/v1): served by whatever provides /mcp, if anything does.
func addMCP(app core.App, h *Host) {
	mcpv1.AddTool(app, mcpv1.Tool{
		Name:  ToolListModules,
		Title: "List WASM modules",
		Description: "Lists the app's WASM modules (pb_hooks/<name>.pb.wasm) by name: for each, whether it can run (or " +
			"why not), its description, version, language, functions, and examples (function, input, output), as its " +
			"manifest says. Call a module from an async JS hook with await $wasm.run.async(name, input) or " +
			"await $wasm.call.async(name, function, input). Superusers only.",
		InputSchema:   map[string]any{"type": "object", "properties": map[string]any{}},
		ReadOnly:      true,
		SuperuserOnly: true,
		Call: func(_ context.Context, a extkit.Auth, _ json.RawMessage) (any, error) {
			if !a.Superuser() {
				return nil, extkit.ErrNotSuperuser
			}
			out := map[string]Info{}
			for _, m := range h.Modules() {
				out[m.Name] = m
			}
			return out, nil
		},
	})
	mcpv1.AddInstructions(app, mcpInstructions)
}
