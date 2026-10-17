package a

import "github.com/prometheus/client_golang/prometheus"

const prefix = "kotoki_realtime_"

func register() {
	// literal
	_ = prometheus.NewCounter(prometheus.CounterOpts{Name: "kotoki_lit_total"})
	// const + literal
	_ = prometheus.NewGauge(prometheus.GaugeOpts{Name: prefix + "const_lit"})
	// const + helper parameter; the call sites supply the names
	counter := func(name string, labels ...string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + name}, labels)
	}
	_ = counter("helper_total", "x")
	_ = counter("helper_two_total")
	// helper parameter alone (db pool style)
	gauge := func(name string) prometheus.GaugeFunc {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name}, func() float64 { return 0 })
	}
	_ = gauge("kotoki_param_gauge")
	// a string that is not a registration must not count
	_ = "kotoki_not_a_metric"
	_ = prometheus.HistogramOpts{Name: "kotoki_hist_seconds"}
}
