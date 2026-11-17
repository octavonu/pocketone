//go:build !nodescribe && !claimrust

package main

import _ "embed"

// describe is what --describe prints: the manifest, kept as manifest.json beside the source.
//
//go:embed manifest.json
var describe string
