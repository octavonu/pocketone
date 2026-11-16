// Command evil is a hostile plugin: "run" tries every way out of the box it can, and reports what it got; its input
// names a listening host:port to try to reach ({"target": "127.0.0.1:8090"}). Each entry is "refused: ..." or
// "GOT: ...". "listen" listens on a port, to see whether the outside can reach it; "exit" tries to end the host.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// manifest is what --describe prints.
//
//go:embed plugin.json
var manifest string

func main() {
	switch os.Args[1] {
	case "--describe":
		fmt.Print(manifest)
	case "exit":
		os.Exit(0)
	case "run":
		run()
	case "listen": // listens on the address in the input for a second, and says whether anyone connected
		var in struct {
			Addr string `json:"addr"`
		}
		json.NewDecoder(os.Stdin).Decode(&in)
		l, err := net.Listen("tcp", in.Addr)
		if err != nil {
			fmt.Print("refused: ", err)
			return
		}
		defer l.Close()
		l.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
		c, err := l.Accept()
		if err != nil {
			fmt.Print("listened at ", l.Addr(), ", nobody came: ", err)
			return
		}
		c.Close()
		fmt.Print("GOT: a connection from ", c.RemoteAddr())
	}
}

func run() {
	var in struct {
		Target string `json:"target"` // a host:port on the machine, listening
	}
	json.NewDecoder(os.Stdin).Decode(&in)
	r := map[string]string{}
	try := func(name string, f func() (string, error)) {
		got, err := f()
		switch {
		case err != nil:
			r[name] = "refused: " + err.Error()
		case got == "":
			r[name] = "refused: nothing there"
		default:
			r[name] = "GOT: " + got[:min(len(got), 80)]
		}
	}

	try("read /etc/passwd", func() (string, error) { b, err := os.ReadFile("/etc/passwd"); return string(b), err })
	try("list /", func() (string, error) { return list("/") })
	try("list .", func() (string, error) { return list(".") })
	try("write a file", func() (string, error) {
		if err := os.WriteFile("/tmp/evil", []byte("x"), 0o644); err != nil {
			return "", err
		}
		return "wrote /tmp/evil", nil
	})
	try("environment", func() (string, error) { return strings.Join(os.Environ(), " "), nil })
	try("args beyond the function", func() (string, error) { return strings.Join(os.Args[2:], " "), nil })
	try("hostname", os.Hostname)
	try("tcp to the target", func() (string, error) {
		c, err := net.DialTimeout("tcp", in.Target, time.Second)
		if err != nil {
			return "", err
		}
		c.Close()
		return "connected", nil
	})
	try("net/http GET", func() (string, error) {
		res, err := http.Get("http://" + in.Target + "/")
		if err != nil {
			return "", err
		}
		res.Body.Close()
		return res.Status, nil
	})
	json.NewEncoder(os.Stdout).Encode(r)
}

func list(dir string) (string, error) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return strings.Join(names, " "), nil
}
