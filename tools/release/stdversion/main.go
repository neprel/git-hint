// Command stdversion prints the baseline HINT data standard version of the client and, on a
// second line, the draft version it writes only with draft fields (release tool).
package main

import (
	"fmt"

	"github.com/neprel/git-hint/internal/version"
)

func main() { fmt.Printf("%s\n%s\n", version.SpecVersion, version.DraftVersion) }
