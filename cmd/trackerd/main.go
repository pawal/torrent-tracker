// Command trackerd tracks the IP addresses of known BitTorrent trackers over
// time and serves the resulting history.
package main

import (
	"os"

	"github.com/pawal/torrent-tracker/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
