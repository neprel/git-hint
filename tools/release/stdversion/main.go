// Command stdversion prints the baseline HINT data standard version of the client and, on a
// second line, the highest conditional draft version it writes (release tool).
package main

import (
	"fmt"

	"github.com/neprel/git-hint/internal/version"
)

func main() { fmt.Printf("%s\n%s\n", version.SpecVersion, version.LatestDraftVersion) }
