// Command berth shares one machine's memory, CPUs, disk and GPUs among jobs and agents.
//
// Invoked under another name (a symlink such as "cargo" in the shim
// directory), it acts as that tool's router. "__supervise" is internal.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/killedoxygen/berth/internal/cli"
	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/shim"
	"github.com/killedoxygen/berth/internal/supervise"
)

var version = "dev"

func main() {
	cli.Version = version
	name := filepath.Base(os.Args[0])
	if name != "berth" && name != "berth.test" {
		os.Exit(runShim(name))
	}
	if len(os.Args) == 3 && os.Args[1] == "__supervise" {
		os.Exit(supervise.Main(os.Args[2]))
	}
	os.Exit(cli.Main(os.Args[1:]))
}

func runShim(tool string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		return 127
	}
	paths := config.UserPaths()
	if _, err := os.Stat(paths.Config); err != nil {
		if _, err := os.Stat(config.SystemPaths().Config); err == nil {
			paths = config.SystemPaths()
		}
	}
	cfg, err := config.Load(paths.Config, probe.Detect())
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth: config:", err, "(running", tool, "with defaults)")
		cfg = config.Default(probe.Detect())
	}
	return shim.Main(&cfg, tool, os.Args[1:], self)
}
