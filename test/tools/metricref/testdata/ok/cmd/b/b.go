package b

import "github.com/prometheus/client_golang/prometheus"

var _ = prometheus.NewSummary(prometheus.SummaryOpts{Name: "kotoki_cmd_summary"})
