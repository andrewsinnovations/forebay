// Command forebay is a daemon-less task queue for scheduling and running commands.
package main

import (
	"os"

	"github.com/andrewsinnovations/forebay/internal/cli"
)

func main() {
	if code := cli.Run(os.Args[1:]); code != 0 {
		os.Exit(code)
	}
}
