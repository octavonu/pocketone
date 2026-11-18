package wasm_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"ganigeorgiev.com/pocketbase/pocketbase/core"
	mcpv1 "github.com/pockethost/pocketone/ext/contracts/mcp/v1"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/wasm"
)

// TestMCPListModules: the app's MCP server (mcp/v1, whoever provides it) gets wasm_list_modules, which gives a
// superuser every module by name with its manifest, and tells agents how to call a module.
func TestMCPListModules(t *testing.T) {
	code := module(t, basic)
	e := newApp(t, wasm.Config{}, func(app core.App, dir string) {
		install(t, dir, "basic", code)
		install(t, dir, "broken", code[:100])
	})

	var tool *mcpv1.Tool
	for _, tl := range mcpv1.Tools(e.app) {
		if tl.Name == wasm.ToolListModules {
			tool = &tl
		}
	}
	if tool == nil {
		t.Fatalf("no %s among the app's MCP tools", wasm.ToolListModules)
	}
	if !tool.ReadOnly || !tool.SuperuserOnly || tool.Destructive {
		t.Fatalf("annotations: %+v", tool)
	}

	got, err := tool.Call(context.Background(), extkit.Auth{Kind: "superuser"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got) // what the agent gets
	var mods map[string]wasm.Info
	if err := json.Unmarshal(raw, &mods); err != nil {
		t.Fatalf("not {name: info}: %s", raw)
	}
	b := mods["basic"]
	if len(mods) != 2 || !b.Running || b.Version != "1.0.0" || b.Description == "" || len(b.Functions) < 2 {
		t.Fatalf("basic: %s", raw)
	}
	if br := mods["broken"]; br.Running || br.Error == "" {
		t.Fatalf("broken: %s", raw)
	}

	for _, a := range []extkit.Auth{{Kind: "record", Collection: "users"}, {}} {
		if _, err := tool.Call(context.Background(), a, nil); !errors.Is(err, extkit.ErrNotSuperuser) {
			t.Fatalf("%q: want ErrNotSuperuser, got %v", a.Kind, err)
		}
	}

	text := strings.Join(mcpv1.Instructions(e.app), "\n")
	for _, want := range []string{"$wasm.run.async(name, input)", "$wasm.call.async(name, function, input)", wasm.ToolListModules} {
		if !strings.Contains(text, want) {
			t.Fatalf("instructions don't say %q:\n%s", want, text)
		}
	}
}
