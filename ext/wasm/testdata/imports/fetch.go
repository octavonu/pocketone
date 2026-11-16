//go:build !extism

package main

import "os"

//go:wasmimport extism:host/user pb_fetch
func pbFetch(req uint64) uint64

func main() { os.Exit(int(pbFetch(0))) }
