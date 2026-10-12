// Command hint-mock-server runs the deterministic mock HINT server of the test kit on a
// loopback port, for offline integration tests with a real agent client. It implements no
// analysis: it returns a fixed-form recommendation for the uploaded snapshot.
// Usage: hint-mock-server -addr-file <file>   (writes the base URL, then serves until killed)
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/neprel/git-hint/internal/testkit"
)

func main() {
	addrFile := flag.String("addr-file", "", "file that receives the base URL")
	flag.Parse()
	srv := testkit.NewServer()
	defer srv.Close()
	if *addrFile != "" {
		if err := os.WriteFile(*addrFile, []byte(srv.URL+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stderr, "hint-mock-server:", srv.URL)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Fprintf(os.Stderr, "hint-mock-server: %d POST requests\n", len(srv.Posted()))
}
