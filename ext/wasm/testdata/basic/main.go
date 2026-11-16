// Command basic is the plugin ext/wasm's core tests run: one function per behavior a host must handle. A plain
// WASI program: the function is os.Args[1], the input is stdin, the output is stdout.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

var count int // how many times this instance ran "counter": always 1 with a fresh instance per call

func main() {
	in, _ := io.ReadAll(os.Stdin)
	switch os.Args[1] {
	case "run": // upper-cases input.text and counts its words
		var v struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(in, &v); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(1)
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"text": strings.ToUpper(v.Text), "words": len(strings.Fields(v.Text))})
	case "echo": // stdin back, byte for byte
		os.Stdout.Write(in)
	case "args":
		json.NewEncoder(os.Stdout).Encode(os.Args)
	case "fail": // a structured error
		fmt.Fprint(os.Stderr, `{"code":"rate_limited","message":"try again later","retryable":true}`)
		os.Exit(1)
	case "failPlain":
		fmt.Fprint(os.Stderr, "something broke")
		os.Exit(1)
	case "failSilent":
		os.Exit(7)
	case "logs": // writes to stderr and works: the app's log gets it
		fmt.Fprintln(os.Stderr, "working on it")
		fmt.Print("done")
	case "loop":
		for {
		}
	case "grab": // 1 GB, a MB at a time
		var hoard [][]byte
		for range 1024 {
			b := make([]byte, 1<<20)
			b[0] = 1
			hoard = append(hoard, b)
		}
		fmt.Print("got 1 GB", len(hoard))
	case "counter":
		count++
		json.NewEncoder(os.Stdout).Encode(map[string]int{"count": count})
	case "panics":
		panic("boom")
	case "big": // 1 MB of output
		os.Stdout.Write(make([]byte, 1<<20))
	case "undeclared": // a function the manifest doesn't list
	default:
		fmt.Fprintf(os.Stderr, `{"code":"no_such_function","message":%q}`, os.Args[1])
		os.Exit(1)
	}
}
