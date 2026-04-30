// demo-parser is a CS2 .dem file parser with two modes:
//
//	demo-parser parse   reads .dem bytes from stdin and writes a
//	                    Result JSON to stdout. Exits non-zero on
//	                    parse error.
//
//	demo-parser server  HTTP service with two endpoints:
//	                      POST /parse       — JSON {demo_url} body;
//	                                          server fetches the demo.
//	                      POST /parse-file  — multipart upload with a
//	                                          "demo" file part.
//
// Default (no arg) is `server` for backwards compatibility with old
// docker images that ENTRYPOINT'd the binary directly.
package main

import (
	"fmt"
	"os"
)

func main() {
	mode := "server"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "parse":
		runCLI()
	case "server":
		runServer()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s (want 'parse' or 'server')\n", mode)
		os.Exit(2)
	}
}
