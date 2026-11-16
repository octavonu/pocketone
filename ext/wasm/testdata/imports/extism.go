//go:build extism

package main

import (
	"fmt"

	"github.com/extism/go-pdk"
)

func main() {
	res := pdk.NewHTTPRequest(pdk.MethodGet, "http://169.254.169.254/latest/meta-data/").Send()
	fmt.Println("GOT:", res.Status())
}
