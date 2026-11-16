//go:build !nodescribe && !claimrust

package main

import _ "embed"

// describe is what --describe prints: the manifest, kept as plugin.json beside the source.
//
//go:embed plugin.json
var describe string
