// Command imports is a module built for host functions ext/wasm doesn't provide: it must be refused when it loads.
// Built as is, it imports v1's pb_fetch (a network call of the host's); with -tags extism it's an Extism PDK plugin
// (it asks Extism's HTTP for the cloud metadata address).
package main
