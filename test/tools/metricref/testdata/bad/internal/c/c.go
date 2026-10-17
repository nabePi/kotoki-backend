package c

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

func register(n string) {
	_ = prometheus.NewCounter(prometheus.CounterOpts{Name: fmt.Sprintf("kotoki_%s_total", n)})
}
