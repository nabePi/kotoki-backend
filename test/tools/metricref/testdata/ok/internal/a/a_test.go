package a

import "github.com/prometheus/client_golang/prometheus"

var _ = prometheus.NewCounter(prometheus.CounterOpts{Name: "kotoki_only_in_test_total"})
