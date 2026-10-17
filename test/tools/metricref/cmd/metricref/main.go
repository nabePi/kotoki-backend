// Command metricref prints the metric names registered in code, one per line (sorted), for
// refreshing deploy/k8s/hack/metrics.txt: go run ./test/tools/metricref/cmd/metricref [-root .]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nabePi/kotoki-backend/test/tools/metricref"
)

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	reg, err := metricref.RegisteredInCode(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, n := range metricref.Sorted(reg) {
		fmt.Println(n)
	}
}
