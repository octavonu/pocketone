// Command example runs one app with ext/wasm and the summary module wired in from pb_hooks: a new post gets its
// summary before it's saved (pb_hooks/main.pb.js). The example folder is the app's folder, laid out like an
// instance on PocketHost: pb_data/ and pb_hooks/ side by side, the module beside the JS hooks.
//
//	cd example && go run . serve --dev
//
// Build the module into pb_hooks/ (before or after starting the app: each call reads it):
//
//	(cd summary && GOOS=wasip1 GOARCH=wasm go build -o ../pb_hooks/summary.pb.wasm .)
//
// Then create a post (dashboard, or the API) and look at its summary field.
//
// Logins (ext/devusers, local testing only): dashboard admin@example.com / password, app user user@example.com / password.
package main

import (
	"log"

	"ganigeorgiev.com/pocketbase/pocketbase"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
	"github.com/pockethost/pocketone/ext/devusers"
	"github.com/pockethost/pocketone/ext/extkit"
	"github.com/pockethost/pocketone/ext/extkit/js"
	"github.com/pockethost/pocketone/ext/jsvm"
	"github.com/pockethost/pocketone/ext/wasm"
)

func main() {
	app := pocketbase.New()
	devusers.Register(app) // local testing: superuser admin@example.com and user user@example.com, password "password"
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		return posts(e.App)
	})
	// the same extension value the standard set gives every pocker instance (set.Add(wasm.New(...)))
	w := wasm.New(wasm.Config{})
	extkit.MustRegister(app, w)
	jsvm.MustRegister(app, jsvm.Config{
		HooksDir: "pb_hooks",
		OnInit:   func(rt js.Runtime) { extkit.BindJS(app, rt, w) }, // $wasm in pb_hooks
	})
	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func posts(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("posts"); err == nil {
		return nil
	}
	c := core.NewBaseCollection("posts")
	c.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "body", Max: 100000},
		&core.TextField{Name: "summary", Max: 1000}, &core.NumberField{Name: "words"})
	return app.Save(c)
}
