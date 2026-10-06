// tether-browse prints the _tether._tcp services on the network: a check of
// the mDNS browser on any OS.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"tetherdesktop/core"
)

func main() {
	b := core.NewBrowser(func(s string) { fmt.Fprintln(os.Stderr, s) })
	b.Interval = 3 * time.Second
	b.Start(func(results []core.Discovered) {
		for _, d := range results {
			fmt.Printf("%-24s %s:%d computer=%v fp=%s…\n", d.Name, strings.Join(d.Hosts, ","), d.Port, d.IsComputer, d.Fingerprint[:16])
		}
		fmt.Println("--")
	})
	time.Sleep(8 * time.Second)
}
