// Command git-hint is the HINT client. Git runs it as `git hint` when it is on PATH.
package main

import (
	"os"
	"runtime/debug"
	"runtime/pprof"

	"github.com/neprel/git-hint/internal/cli"
)

func main() {
	dir, err := os.Getwd()
	if err != nil {
		os.Stderr.WriteString("git hint: " + err.Error() + "\n")
		os.Exit(1)
	}
	// Large snapshots allocate many short-lived maps; a higher GC target trades memory
	// for time. GOGC in the environment still overrides it.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}
	// HINT_CPUPROFILE=<file> writes a CPU profile (troubleshooting slow scans).
	if p := os.Getenv("HINT_CPUPROFILE"); p != "" {
		if f, err := os.Create(p); err == nil && pprof.StartCPUProfile(f) == nil {
			code := run(dir)
			pprof.StopCPUProfile()
			f.Close()
			os.Exit(code)
		}
	}
	os.Exit(run(dir))
}

func run(dir string) int {
	return (cli.Run(os.Args[1:], &cli.Env{Dir: dir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		IsTerminal: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		}}))
}
