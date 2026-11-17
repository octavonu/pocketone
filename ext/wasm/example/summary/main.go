// Command summary is an example ext/wasm module: a pure function from a post to its summary, written as a plain
// command-line program.
//
//	stdin:  {"title": "...", "body": "..."}
//	stdout: {"text": "<title>: <first 12 words> …", "words": <word count>}
//
// It reads nothing and writes nothing else: the app's pb_hooks give it the post and save what it returns (see
// ../pb_hooks/main.pb.js). It fails by exiting non-zero, with a JSON error on stderr that pb_hooks get as
// err.code / err.message. Build it with
//
//	GOOS=wasip1 GOARCH=wasm go build -o summary.wasm .
//
// and try it without PocketBase: echo '{"title":"Hi","body":"one two"}' | wazero run summary.wasm
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// manifest is what `<name> --describe` prints: manifest.json beside the source.
//
//go:embed manifest.json
var manifest string

func main() {
	switch os.Args[1] { // the function asked for: --describe prints the manifest; "run" is the only other
	case "--describe":
		fmt.Print(manifest)
		return
	case "run":
	default:
		fail("no_such_function", os.Args[1])
	}
	var post struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&post); err != nil {
		fail("bad_input", "the input isn't a post: "+err.Error())
	}
	if strings.TrimSpace(post.Body) == "" {
		fail("empty_body", "the post has no body to summarize")
	}
	words := strings.Fields(post.Body)
	short := strings.Join(words[:min(len(words), 12)], " ")
	if len(words) > 12 {
		short += " …"
	}
	text := short
	if post.Title != "" {
		text = post.Title + ": " + short
	}
	json.NewEncoder(os.Stdout).Encode(map[string]any{"text": text, "words": len(words)})
}

// fail ends the program with an error pb_hooks can read.
func fail(code, message string) {
	b, _ := json.Marshal(map[string]string{"code": code, "message": message})
	fmt.Fprint(os.Stderr, string(b))
	os.Exit(1)
}
