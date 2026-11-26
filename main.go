// Command fold is the one binary: the JSON API, the web client and the CLI.
package main

import (
	"embed"
	"os"

	"fold/internal/app"
)

// version is set at build time with -ldflags "-X main.version=x.y.z".
var version = "dev"

//go:embed web/dist
var dist embed.FS

//go:embed docs/MEMBERS.md
var memberHelp []byte

func main() {
	os.Exit(app.Main(app.Build{Version: version, Dist: dist, MemberHelp: memberHelp}, os.Args[1:], os.Stdout, os.Stderr))
}
