package wasm_test

// The modules the tests run: Go plugin folders built for wasip1 (once per test run), the Rust and Zig examples kept
// built beside their sources (prebuilt), and a few modules written byte by byte (tiny, says, producers).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildMu sync.Mutex
	built   = map[string][]byte{}
)

// module compiles a plugin folder (once per test run) and returns its .wasm.
func module(t *testing.T, src string) []byte { return build(t, src, "", false) }

// moduleTags builds a Go plugin folder with build tags.
func moduleTags(t *testing.T, src, tags string) []byte { return build(t, src, tags, false) }

// build builds a Go plugin folder for wasip1: a program (what a plugin is), or a library (-buildmode=c-shared).
func build(t *testing.T, src, tags string, library bool) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm plugins")
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	key := fmt.Sprint(src, " ", tags, " ", library)
	if b, ok := built[key]; ok {
		return b
	}
	out := filepath.Join(t.TempDir(), "plugin.wasm")
	args := []string{"build", "-tags=" + tags, "-o", out}
	if library {
		args = append(args, "-buildmode=c-shared")
	}
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "GOTOOLCHAIN=auto")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", src, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	built[key] = b
	return b
}

// The Rust and Zig examples are kept built, beside their sources (example/<folder>/<name>.pb.wasm), so every run of
// the tests runs them, with no Rust or Zig installed. A rebuild gives the same bytes with the same compiler:
// rustVersion and zigVersion, the versions they're built with. `go test -run TestPrebuiltExamples -update` rebuilds
// them.
const (
	rustVersion = "1.100.0"
	zigVersion  = "0.14.0"
)

// prebuilt lists them: the folder, the language, what the build makes, and the file kept (the plugin's name).
var prebuilt = []struct{ dir, lang, out, file string }{
	{"example/summary-rust", "rust", "summary", "summary.pb.wasm"},
	{"example/summary-zig", "zig", "summary", "summary.pb.wasm"},
	{"example/edifact-orders-rust", "rust", "orders", "edifact-orders-rust.pb.wasm"},
	{"example/order-xml", "rust", "order-xml", "order-xml.pb.wasm"},
	{"example/waflows", "rust", "waflows", "waflows.pb.wasm"},
	{"example/devil", "rust", "devil", "devil.pb.wasm"},
	{"example/memhog", "rust", "memhog", "memhog.pb.wasm"},
}

// prebuiltModule is the kept build of an example folder.
func prebuiltModule(t *testing.T, dir string) []byte {
	t.Helper()
	for _, p := range prebuilt {
		if p.dir == dir {
			b, err := os.ReadFile(filepath.Join(p.dir, p.file))
			if err != nil {
				t.Fatalf("%v (go test -run TestPrebuiltExamples -update builds it)", err)
			}
			return b
		}
	}
	t.Fatalf("%s isn't a prebuilt example", dir)
	return nil
}

// toolchain skips the test, or with -update fails it, when the compiler isn't there or isn't the version the
// examples are built with: another version builds other bytes.
func toolchain(t *testing.T, name, want string, version ...string) string {
	t.Helper()
	stop := t.Skipf
	if *update {
		stop = t.Fatalf
	}
	path, err := exec.LookPath(name)
	if err != nil && name == "zig" {
		home, _ := os.UserHomeDir()
		if found, _ := filepath.Glob(filepath.Join(home, ".local", "zig-*", "zig")); len(found) > 0 {
			path, err = found[len(found)-1], nil
		}
	}
	if err != nil {
		stop("no %s", name)
		return ""
	}
	out, err := exec.Command(path, version...).Output()
	if got := strings.TrimSpace(string(out)); err != nil || !strings.Contains(got, want) {
		stop("%s is %q; the prebuilt examples are built with %s", name, got, want)
	}
	return path
}

// buildExample builds a prebuilt example from its source, as the kept file was built. Rust's paths are remapped
// (the toolchain's sources to /rustc/<commit>, as in Rust's own builds; the crates to /cargo), so the module holds
// no path of the machine that built it.
func buildExample(t *testing.T, dir string) []byte {
	t.Helper()
	if testing.Short() {
		t.Skip("builds wasm plugins")
	}
	var p struct{ dir, lang, out, file string }
	for _, q := range prebuilt {
		if q.dir == dir {
			p = q
		}
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if b, ok := built[dir]; ok {
		return b
	}
	var cmd *exec.Cmd
	var out string
	switch p.lang {
	case "rust":
		toolchain(t, "rustc", "rustc "+rustVersion+" ", "--version")
		cargo := toolchain(t, "cargo", "cargo "+rustVersion+" ", "--version")
		sysroot, _ := exec.Command("rustc", "--print", "sysroot").Output()
		info, _ := exec.Command("rustc", "-vV").Output()
		_, commit, _ := strings.Cut(string(info), "commit-hash: ")
		commit, _, _ = strings.Cut(commit, "\n")
		cargoHome := os.Getenv("CARGO_HOME")
		if cargoHome == "" {
			home, _ := os.UserHomeDir()
			cargoHome = filepath.Join(home, ".cargo")
		}
		flags := []string{
			"--remap-path-prefix=" + filepath.Join(strings.TrimSpace(string(sysroot)), "lib/rustlib/src/rust") + "=/rustc/" + commit,
			"--remap-path-prefix=" + filepath.Join(cargoHome, "registry/src") + "=/cargo",
		}
		cmd = exec.Command(cargo, "build", "--release", "--locked", "--target", "wasm32-wasip1")
		cmd.Env = append(os.Environ(), "CARGO_ENCODED_RUSTFLAGS="+strings.Join(flags, "\x1f"))
		out = filepath.Join(dir, "target/wasm32-wasip1/release", p.out+".wasm")
	case "zig":
		zig := toolchain(t, "zig", zigVersion, "version")
		cmd = exec.Command(zig, "build", "-Doptimize=ReleaseSmall")
		out = filepath.Join(dir, "zig-out/bin", p.out+".wasm")
	default:
		t.Fatalf("%s isn't a prebuilt example", dir)
	}
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", dir, err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	built[dir] = b
	return b
}

// tiny is a WASI program made by hand, a few dozen bytes: a _start that returns at once, or, with spin, one that
// loops until the call's time limit. pad bytes in a custom section make the module as big as a test needs.
func tiny(spin bool, pad int) []byte {
	body := []byte{0x00, 0x0b} // no locals; end
	if spin {
		body = []byte{0x00, 0x03, 0x40, 0x0c, 0x00, 0x0b, 0x0b} // no locals; loop: br 0; end; end
	}
	sec := func(id byte, content []byte) []byte {
		return append(append([]byte{id}, uleb(len(content))...), content...)
	}
	b := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	b = append(b, sec(1, []byte{0x01, 0x60, 0x00, 0x00})...)                              // one type: () -> ()
	b = append(b, sec(3, []byte{0x01, 0x00})...)                                          // one function of it
	b = append(b, sec(7, append(append([]byte{0x01, 0x06}, "_start"...), 0x00, 0x00))...) // exported as _start
	b = append(b, sec(10, append([]byte{0x01}, append(uleb(len(body)), body...)...))...)  // its body
	if pad > 0 {
		b = append(b, sec(0, append([]byte{0x03, 'p', 'a', 'd'}, make([]byte, pad)...))...)
	}
	return b
}

// says is a module that prints out on stdout and exits 0, whatever it's asked: a plugin whose --describe prints out.
func says(out string) []byte {
	sec := func(id byte, content []byte) []byte {
		return append(append([]byte{id}, uleb(len(content))...), content...)
	}
	str := func(s string) []byte { return append(uleb(len(s)), s...) }
	le := func(n int) []byte { return []byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)} }
	b := []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00}
	// types: fd_write's (i32, i32, i32, i32) -> i32, and () -> ()
	b = append(b, sec(1, []byte{0x02, 0x60, 0x04, 0x7f, 0x7f, 0x7f, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x00})...)
	b = append(b, sec(2, append(append(append([]byte{0x01}, str("wasi_snapshot_preview1")...), str("fd_write")...), 0x00, 0x00))...)
	b = append(b, sec(3, []byte{0x01, 0x01})...)                                       // _start, of type 1
	b = append(b, sec(5, append([]byte{0x01, 0x00}, uleb((16+len(out))>>16+1)...))...) // memory: enough pages
	b = append(b, sec(7, append(append(append(append([]byte{0x02}, str("_start")...), 0x00, 0x01), str("memory")...), 0x02, 0x00))...)
	// _start: fd_write(1, iovec at 0, 1 iovec, nwritten at 8); drop
	body := []byte{0x00, 0x41, 0x01, 0x41, 0x00, 0x41, 0x01, 0x41, 0x08, 0x10, 0x00, 0x1a, 0x0b}
	b = append(b, sec(10, append([]byte{0x01}, append(uleb(len(body)), body...)...))...)
	// data at 0: the iovec {16, len(out)}, 8 bytes for nwritten, then out
	data := append(append(append(le(16), le(len(out))...), make([]byte, 8)...), out...)
	b = append(b, sec(11, append(append([]byte{0x01, 0x00, 0x41, 0x00, 0x0b}, uleb(len(data))...), data...))...)
	return b
}

// uleb is n in unsigned LEB128, as wasm writes sizes.
func uleb(n int) []byte {
	var b []byte
	for {
		c := byte(n & 0x7f)
		if n >>= 7; n == 0 {
			return append(b, c)
		}
		b = append(b, c|0x80)
	}
}

// producers builds a producers section: field name, then name/version pairs, then the next field name, ...
func producers(parts ...string) []byte {
	type field struct {
		name   string
		values [][2]string
	}
	var fields []field
	for i := 0; i < len(parts); {
		f := field{name: parts[i]}
		i++
		for i+1 < len(parts) && parts[i] != "processed-by" && parts[i] != "language" {
			f.values = append(f.values, [2]string{parts[i], parts[i+1]})
			i += 2
		}
		fields = append(fields, f)
	}
	str := func(b []byte, s string) []byte { return append(append(b, byte(len(s))), s...) }
	b := []byte{byte(len(fields))}
	for _, f := range fields {
		b = append(str(b, f.name), byte(len(f.values)))
		for _, v := range f.values {
			b = str(str(b, v[0]), v[1])
		}
	}
	return b
}
