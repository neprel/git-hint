// Command stdversion prints the HINT data standard version of the client (release tool).
package main

import (
	"fmt"

	"github.com/neprel/git-hint/internal/version"
)

func main() { fmt.Println(version.SpecVersion) }
