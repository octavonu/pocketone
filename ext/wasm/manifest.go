package wasm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Manifest is what a plugin says about itself: what it prints when run as `<name> --describe` (the function asked
// for is "--describe", with nothing on stdin). Every field is optional, and so is answering: a plugin that exits
// non-zero or prints nothing gets the defaults (functions ["run"]).
//
//	{
//	  "description": "Summarizes a post",
//	  "version": "1.2.0",
//	  "functions": ["run"],
//	  "language": "Rust",
//	  "repository": "https://github.com/you/summary",
//	  "examples": [{"function": "run", "input": {"title": "Hi", "body": "one two"}, "output": {"text": "Hi: one two", "words": 2}}]
//	}
//
// There is nothing to ask for: a plugin gets its input and nothing else. The manifest is only shown
// ($wasm.plugins()): calls don't read it (the plugin answers a function it doesn't have itself, by convention with
// no_such_function), nothing runs the examples, and nothing fetches the repository.
type Manifest struct {
	Description string    `json:"description,omitempty"`
	Version     string    `json:"version,omitempty"`    // the author's version, as they write it (1.2.0, 2026-11-15, ...)
	Functions   []string  `json:"functions,omitempty"`  // what the plugin takes as its first argument; default ["run"]
	Language    string    `json:"language,omitempty"`   // what it's written in, when the module doesn't say (see Info)
	Repository  string    `json:"repository,omitempty"` // where its source is: an http(s) URL
	Examples    []Example `json:"examples,omitempty"`   // inputs to try it with, and what they give
}

// Example is one call to try a plugin with, the same as from pb_hooks: $wasm.call(name, function, input). A JSON
// string input is the plugin's stdin as it is (raw text: EDIFACT, XML, CSV); anything else is sent as JSON.
type Example struct {
	Function string          `json:"function,omitempty"` // default "run"
	Input    json.RawMessage `json:"input"`
	Output   json.RawMessage `json:"output,omitempty"` // what it gives, when the example says
}

// describeFlag is the function a plugin is asked for to print its manifest. Function names can't start with "-", so
// it's never one of a plugin's own.
const describeFlag = "--describe"

const (
	maxManifest   = 64 << 10 // bytes
	maxExamples   = 16
	maxRepository = 256
)

var (
	validLanguage = regexp.MustCompile(`^[A-Za-z0-9+#. -]{1,32}$`)
	validVersion  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_-]{0,31}$`)
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// functions is the declared functions, or the default.
func (m Manifest) functions() []string {
	if len(m.Functions) == 0 {
		return []string{"run"}
	}
	return m.Functions
}

func (m *Manifest) validate() error {
	for _, f := range m.Functions {
		if !validName.MatchString(f) || strings.HasPrefix(f, "-") {
			return fmt.Errorf("functions: %q isn't a valid function name (letters, digits, - and _, not starting with -)", f)
		}
	}
	if m.Version != "" && !validVersion.MatchString(m.Version) {
		return fmt.Errorf("version: %q isn't a version (up to 32 letters, digits and .+_-)", m.Version)
	}
	if m.Language != "" && !validLanguage.MatchString(m.Language) {
		return fmt.Errorf("language: %q isn't a language name (up to 32 letters, digits, spaces and +#.-)", m.Language)
	}
	if m.Repository != "" {
		if err := checkRepository(m.Repository); err != nil {
			return fmt.Errorf("repository: %w", err)
		}
	}
	if len(m.Examples) > maxExamples {
		return fmt.Errorf("examples: %d, at most %d", len(m.Examples), maxExamples)
	}
	for i := range m.Examples {
		e := &m.Examples[i]
		if e.Function == "" {
			e.Function = "run"
		}
		if !slices.Contains(m.functions(), e.Function) {
			return fmt.Errorf("examples[%d]: function %q isn't in functions", i, e.Function)
		}
		if len(e.Input) == 0 {
			return fmt.Errorf("examples[%d]: no input", i)
		}
	}
	return nil
}

// checkRepository accepts a plain http(s) URL with a host: it's shown as a link, so never javascript: or data:, and
// never credentials.
func checkRepository(s string) error {
	if len(s) > maxRepository {
		return fmt.Errorf("over %d characters", maxRepository)
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		strings.ContainsAny(s, " \t\n\"'<>") {
		return fmt.Errorf("%q isn't an http(s) URL", s)
	}
	return nil
}

// Stdin is what the plugin reads when this example runs: a JSON string's text as it is, anything else as JSON.
func (e Example) Stdin() []byte {
	var s string
	if json.Unmarshal(e.Input, &s) == nil {
		return []byte(s)
	}
	return e.Input
}

// parseManifest reads what a plugin printed for --describe; the zero Manifest when empty. Unknown fields are
// refused, so a manifest that expects more (dependencies, permissions) isn't silently run without it, and so is anything after the object.
func parseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if len(bytes.TrimSpace(raw)) == 0 {
		return m, nil
	}
	if len(raw) > maxManifest {
		return m, fmt.Errorf("over %d KB", maxManifest>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, errors.New("not a plugin manifest: " + err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return m, errors.New("not a plugin manifest: more after the JSON object")
	}
	return m, m.validate()
}
