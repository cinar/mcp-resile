// Command mcp-resile is the resilience gateway/reverse proxy for the
// Model Context Protocol.
package main

import (
	"fmt"
	"os"

	"github.com/cinar/mcp-resile/internal/version"
)

func main() {
	fmt.Fprintf(os.Stdout, "mcp-resile %s\n", version.Version)
}
