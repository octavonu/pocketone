//go:build nodescribe

package main

// describe is empty: built with -tags nodescribe, the module doesn't answer --describe.
var describe string
