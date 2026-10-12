// Command hintcheck validates HINT v0.1 documents: JSON Schema, then the semantic rules of
// hint-data §9.2. It prints one line per file and exits 1 if any file is invalid.
//
// Usage (from tests/conformance):
//
//	go run ./cmd/hintcheck [-schemas ../../schemas/v0.1] [-policy policy.json] file.json...
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/neprel/git-hint/tests/conformance/hintconf"
)

func main() {
	dir := flag.String("schemas", "../../schemas/v0.1", "schema directory")
	polFile := flag.String("policy", "", "export_policy document for the field allowlist check (optional)")
	flag.Parse()
	s, err := hintconf.LoadSchemas(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var policy map[string]any
	if *polFile != "" {
		_, doc, err := read(*polFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		policy, _ = doc["payload"].(map[string]any)
	}
	bad := 0
	for _, f := range flag.Args() {
		dec, doc, err := read(f)
		switch {
		case err != nil:
			fmt.Printf("%s: not JSON: %v\n", f, err)
			bad++
		case s.Validate(dec) != nil:
			fmt.Printf("%s: schema_invalid: %v\n", f, s.Validate(dec))
			bad++
		default:
			if fs := hintconf.Check(doc, policy); len(fs) > 0 {
				fmt.Printf("%s: %v\n", f, fs)
				bad++
			} else {
				fmt.Printf("%s: valid\n", f)
			}
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
}

func read(path string) (any, map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	dec, err := hintconf.Decode(b)
	if err != nil {
		return nil, nil, err
	}
	doc, err := hintconf.Plain(dec)
	return dec, doc, err
}
