// Command dashgen renders the six Grafana dashboards of spec §13.7.4 from the panel specs in
// panels.go: go run ./test/tools/dashgen -out deploy/k8s/base/observability/dashboards
//
// The output is deterministic (encoding/json over structs, no timestamps), so a second run
// produces identical bytes and `make dashboards-check` can compare against git.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Panel is one Grafana panel: a title, a Grafana panel type, a unit and one PromQL expression per
// target.
type Panel struct {
	Title, Type, Unit string
	Exprs             []string
}

// Dashboard is one Grafana dashboard.
type Dashboard struct {
	UID, Title string
	Panels     []Panel
}

const (
	dsVar         = "${DS_PROMETHEUS}"
	schemaVersion = 39
	gridCols      = 24
)

type dsRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type target struct {
	RefID        string `json:"refId"`
	Expr         string `json:"expr"`
	Datasource   dsRef  `json:"datasource"`
	Format       string `json:"format,omitempty"`
	LegendFormat string `json:"legendFormat"`
	Range        bool   `json:"range"`
}

type gridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type fieldDefaults struct {
	Unit string `json:"unit"`
}

type fieldConfig struct {
	Defaults  fieldDefaults `json:"defaults"`
	Overrides []struct{}    `json:"overrides"`
}

type heatmapOptions struct {
	Calculate bool `json:"calculate"`
}

type jsonPanel struct {
	ID          int         `json:"id"`
	Type        string      `json:"type"`
	Title       string      `json:"title"`
	Datasource  dsRef       `json:"datasource"`
	GridPos     gridPos     `json:"gridPos"`
	FieldConfig fieldConfig `json:"fieldConfig"`
	Options     any         `json:"options,omitempty"`
	Targets     []target    `json:"targets"`
}

type variable struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Type    string `json:"type"`
	Query   string `json:"query"`
	Refresh int    `json:"refresh"`
	Hide    int    `json:"hide"`
}

type templating struct {
	List []variable `json:"list"`
}

type timeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type jsonDashboard struct {
	UID           string      `json:"uid"`
	Title         string      `json:"title"`
	Tags          []string    `json:"tags"`
	SchemaVersion int         `json:"schemaVersion"`
	Editable      bool        `json:"editable"`
	Timezone      string      `json:"timezone"`
	Refresh       string      `json:"refresh"`
	Time          timeRange   `json:"time"`
	Templating    templating  `json:"templating"`
	Panels        []jsonPanel `json:"panels"`
}

// panelSize returns the grid width and height of a panel type (24-column grid; two timeseries
// side by side, four stats per row, a heatmap across the full row).
func panelSize(typ string) (w, h int) {
	switch typ {
	case "stat":
		return 6, 4
	case "heatmap":
		return 24, 9
	default:
		return 12, 8
	}
}

// Render returns the Grafana JSON of d (schema 39+), indented, newline-terminated. Panels flow
// left to right in the order given and wrap at 24 columns.
func Render(d Dashboard) ([]byte, error) {
	out := jsonDashboard{
		UID: d.UID, Title: d.Title, Tags: []string{"kotoki"}, SchemaVersion: schemaVersion,
		Editable: false, Timezone: "browser", Refresh: "30s", Time: timeRange{From: "now-6h", To: "now"},
		Templating: templating{List: []variable{{
			Name: "DS_PROMETHEUS", Label: "Prometheus", Type: "datasource", Query: "prometheus", Refresh: 1,
		}}},
		Panels: make([]jsonPanel, 0, len(d.Panels)),
	}
	ds := dsRef{Type: "prometheus", UID: dsVar}
	x, y, rowH := 0, 0, 0
	for i, p := range d.Panels {
		w, h := panelSize(p.Type)
		if x+w > gridCols {
			x, y, rowH = 0, y+rowH, 0
		}
		jp := jsonPanel{
			ID: i + 1, Type: p.Type, Title: p.Title, Datasource: ds,
			GridPos:     gridPos{H: h, W: w, X: x, Y: y},
			FieldConfig: fieldConfig{Defaults: fieldDefaults{Unit: p.Unit}, Overrides: []struct{}{}},
		}
		if p.Type == "heatmap" {
			jp.Options = heatmapOptions{Calculate: false}
		}
		for j, e := range p.Exprs {
			tg := target{RefID: string(rune('A' + j)), Expr: e, Datasource: ds, LegendFormat: "__auto", Range: true}
			if p.Type == "heatmap" {
				tg.Format = "heatmap"
				tg.LegendFormat = "{{le}}"
			}
			jp.Targets = append(jp.Targets, tg)
		}
		out.Panels = append(out.Panels, jp)
		x += w
		rowH = max(rowH, h)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// fileNames maps a dashboard UID to its published file name.
var fileNames = map[string]string{
	"kotoki-overview":         "kotoki-overview.json",
	"kotoki-realtime-latency": "realtime-latency.json",
	"kotoki-providers":        "providers.json",
	"kotoki-async":            "async.json",
	"kotoki-data-stores":      "data-stores.json",
	"kotoki-business":         "business-integrity.json",
}

// run renders every dashboard into dir (created if absent).
func run(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	for _, d := range All() {
		name, ok := fileNames[d.UID]
		if !ok {
			return fmt.Errorf("dashgen: no file name for dashboard uid %q", d.UID)
		}
		b, err := Render(d)
		if err != nil {
			return fmt.Errorf("dashgen: %s: %w", d.Title, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	out := flag.String("out", "deploy/k8s/base/observability/dashboards", "output directory")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
