package promtext

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

const exposition = `# HELP foo_total Some "help" text with {braces} and a \ backslash
# TYPE foo_total counter
foo_total{a="b",c="d,e"} 1.5
foo_total{a="x\"y\\z\nw",} 2 1700000000000

bar 3
inf_bucket{le="+Inf"} +Inf
neg -Inf
nan_metric NaN
spaced{ a = "1" , b="}{ x" }   7
sci 1.5e3
`

func TestParse_Exposition(t *testing.T) {
	ss, err := Parse(strings.NewReader(exposition))
	require.NoError(t, err)
	require.Len(t, ss, 8)

	require.Equal(t, Sample{Name: "foo_total", Labels: map[string]string{"a": "b", "c": "d,e"}, Value: 1.5}, ss[0])
	require.Equal(t, map[string]string{"a": "x\"y\\z\nw"}, ss[1].Labels)
	require.Equal(t, 2.0, ss[1].Value) // timestamp ignored
	require.Equal(t, Sample{Name: "bar", Labels: map[string]string{}, Value: 3}, ss[2])
	require.True(t, math.IsInf(ss[3].Value, 1))
	require.Equal(t, "+Inf", ss[3].Labels["le"])
	require.True(t, math.IsInf(ss[4].Value, -1))
	require.True(t, math.IsNaN(ss[5].Value))
	require.Equal(t, map[string]string{"a": "1", "b": "}{ x"}, ss[6].Labels)
	require.Equal(t, 7.0, ss[6].Value)
	require.Equal(t, 1500.0, ss[7].Value)
	require.Equal(t, "sci", ss[7].Name)
	empty, err := Parse(strings.NewReader("empty{} 1\n"))
	require.NoError(t, err)
	require.Equal(t, Sample{Name: "empty", Labels: map[string]string{}, Value: 1}, empty[0])
}

func TestParse_CRLFAndNoTrailingNewline(t *testing.T) {
	ss, err := Parse(strings.NewReader("a 1\r\nb{x=\"y\"} 2"))
	require.NoError(t, err)
	require.Len(t, ss, 2)
	require.Equal(t, 2.0, ss[1].Value)
}

func TestParse_ReadError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Parse(io.MultiReader(strings.NewReader("a 1\n"), iotest.ErrReader(boom)))
	require.ErrorIs(t, err, boom)
}

func TestParse_Empty(t *testing.T) {
	ss, err := Parse(strings.NewReader(""))
	require.NoError(t, err)
	require.Empty(t, ss)
	ss, err = Parse(strings.NewReader("# only comments\n\n"))
	require.NoError(t, err)
	require.Empty(t, ss)
}

func TestParse_VeryLongLine(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	ss, err := Parse(strings.NewReader(`m{l="` + long + `"} 1` + "\n"))
	require.NoError(t, err)
	require.Len(t, ss, 1)
	require.Len(t, ss[0].Labels["l"], 200_000)
}

func TestParse_Errors(t *testing.T) {
	bad := map[string]string{
		"no value":             "metric",
		"no value with labels": `metric{a="b"}`,
		"bad value":            "metric abc",
		"bad timestamp":        "metric 1 notatime",
		"three fields":         "metric 1 2 3",
		"unterminated labels":  `metric{a="b"`,
		"unterminated quote":   `metric{a="b} 1`,
		"bad escape":           `metric{a="\q"} 1`,
		"label without eq":     `metric{a} 1`,
		"unquoted value":       `metric{a=b} 1`,
		"missing comma":        `metric{a="b" c="d"} 1`,
		"bad metric name":      "9metric 1",
		"empty name":           `{a="b"} 1`,
		"bad label name":       `metric{1a="b"} 1`,
		"junk after labels":    `metric{a="b"}x 1`,
	}
	for name, line := range bad {
		_, err := Parse(strings.NewReader("ok 1\n" + line + "\n"))
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "line 2", name)
	}
}

func TestSum(t *testing.T) {
	ss, err := Parse(strings.NewReader(`
m{a="1",b="x"} 1
m{a="1",b="y"} 2
m{a="2",b="x"} 4
other{a="1"} 100
`))
	require.NoError(t, err)
	require.Equal(t, 7.0, ss.Sum("m", nil))
	require.Equal(t, 3.0, ss.Sum("m", map[string]string{"a": "1"}))
	require.Equal(t, 5.0, ss.Sum("m", map[string]string{"b": "x"}))
	require.Equal(t, 1.0, ss.Sum("m", map[string]string{"a": "1", "b": "x"}))
	require.Equal(t, 0.0, ss.Sum("m", map[string]string{"a": "9"}))
	require.Equal(t, 0.0, ss.Sum("nope", nil))
	require.Equal(t, 0.0, ss.Sum("m", map[string]string{"zzz": ""}), "an absent label never matches")
}

func TestFilterAndSumBy(t *testing.T) {
	ss, _ := Parse(strings.NewReader("m{r=\"a\"} 1\nm{r=\"b\"} 2\nm{r=\"a\"} 3\nn 9\n"))
	got := ss.SumBy("m", nil, "r")
	require.Equal(t, map[string]float64{"a": 4, "b": 2}, got)
	require.Equal(t, map[string]float64{"b": 2}, ss.SumBy("m", map[string]string{"r": "b"}, "r"))
	require.Empty(t, ss.SumBy("zzz", nil, "r"))
	f := ss.Filter(func(s Sample) bool { return s.Labels["r"] == "a" })
	require.Len(t, f, 2)
	require.Len(t, ss, 4, "Filter does not modify the receiver")
}

func TestHasPrefix(t *testing.T) {
	ss, _ := Parse(strings.NewReader("kotoki_realtime_x 1\nother 2\n"))
	require.True(t, ss.HasPrefix("kotoki_realtime_"))
	require.False(t, ss.HasPrefix("kotoki_worker_"))
	require.False(t, Samples(nil).HasPrefix("a"))
}

// bucketText renders a cumulative histogram: les and cum are parallel; le "+Inf" is the last.
func bucketText(name, labels string, les []string, cum []float64) string {
	var b strings.Builder
	for i, le := range les {
		sep := ""
		if labels != "" {
			sep = labels + ","
		}
		b.WriteString(name + `_bucket{` + sep + `le="` + le + `"} ` + strconv.FormatFloat(cum[i], 'f', -1, 64) + "\n")
	}
	return b.String()
}

var les = []string{"0.1", "0.2", "0.5", "+Inf"}

func mustParse(t *testing.T, s string) Samples {
	t.Helper()
	ss, err := Parse(strings.NewReader(s))
	require.NoError(t, err)
	return ss
}

func TestQuantile_DeltaAcrossInstances(t *testing.T) {
	// instance A: before 10/20/30/30, after 20/60/90/100 -> delta 10/40/60/70
	// instance B: before 0, after 10/40/90/100          -> delta 10/40/90/100
	// summed cumulative delta: 20/80/150/170
	before := mustParse(t, bucketText("lat", `inst="a"`, les, []float64{10, 20, 30, 30})+
		bucketText("lat", `inst="b"`, les, []float64{0, 0, 0, 0}))
	after := mustParse(t, bucketText("lat", `inst="a"`, les, []float64{20, 60, 90, 100})+
		bucketText("lat", `inst="b"`, les, []float64{10, 40, 90, 100}))

	v, ok := Quantile(0.5, before, after, "lat", nil) // rank 85: 0.2 + 0.3*(85-80)/(150-80)
	require.True(t, ok)
	require.InDelta(t, 0.2+0.3*5.0/70.0, v, 1e-9)

	v, ok = Quantile(0.05, before, after, "lat", nil) // rank 8.5 in the first bucket: 0 + 0.1*8.5/20
	require.True(t, ok)
	require.InDelta(t, 0.0425, v, 1e-9)

	// rank 161.5 lands in +Inf: Prometheus answers with the highest finite bound
	v, ok = Quantile(0.95, before, after, "lat", nil)
	require.True(t, ok)
	require.Equal(t, 0.5, v)

	// the bucket counts of "before" really are subtracted: without them the median differs
	vNoBefore, ok := Quantile(0.5, nil, after, "lat", nil)
	require.True(t, ok)
	require.NotEqual(t, v, vNoBefore)
	// after alone: cumulative 30/100/180/200, rank 100 sits exactly at the top of the 0.2 bucket
	require.InDelta(t, 0.2, vNoBefore, 1e-9)
}

func TestQuantile_MatchSelectsSeries(t *testing.T) {
	text := bucketText("lat", `kind="reply"`, les, []float64{0, 100, 100, 100}) +
		bucketText("lat", `kind="opening"`, les, []float64{0, 0, 0, 100})
	after := mustParse(t, text)
	v, ok := Quantile(0.95, nil, after, "lat", map[string]string{"kind": "reply"})
	require.True(t, ok)
	require.InDelta(t, 0.1+0.1*0.95, v, 1e-9)
	_, ok = Quantile(0.95, nil, after, "lat", map[string]string{"kind": "nothing"})
	require.False(t, ok)
	_, ok = Quantile(0.95, nil, after, "other", nil)
	require.False(t, ok)
}

func TestQuantile_NoObservationsOrBadData(t *testing.T) {
	same := mustParse(t, bucketText("lat", "", les, []float64{1, 2, 3, 4}))
	_, ok := Quantile(0.95, same, same, "lat", nil)
	require.False(t, ok, "count delta 0")

	// counter reset: a bucket went down. The delta cannot be trusted.
	reset := mustParse(t, bucketText("lat", "", les, []float64{5, 6, 7, 8}))
	more := mustParse(t, bucketText("lat", "", les, []float64{0, 0, 0, 20}))
	_, ok = Quantile(0.95, reset, more, "lat", nil)
	require.False(t, ok)

	noInf := mustParse(t, bucketText("lat", "", les[:3], []float64{1, 2, 3}))
	_, ok = Quantile(0.5, nil, noInf, "lat", nil)
	require.False(t, ok, "without +Inf the total is unknown")

	_, ok = Quantile(0.5, nil, nil, "lat", nil)
	require.False(t, ok)
	good := mustParse(t, bucketText("lat", "", les, []float64{1, 2, 3, 4}))
	for _, q := range []float64{-0.1, 1.1, math.NaN()} {
		_, ok = Quantile(q, nil, good, "lat", nil)
		require.False(t, ok, "q=%v", q)
	}
}

func TestQuantile_EdgeBuckets(t *testing.T) {
	// everything in the first bucket whose lower bound is 0
	one := mustParse(t, bucketText("lat", "", les, []float64{10, 10, 10, 10}))
	v, ok := Quantile(1, nil, one, "lat", nil)
	require.True(t, ok)
	require.InDelta(t, 0.1, v, 1e-9)

	// a non-positive first bound: Prometheus returns that bound
	neg := mustParse(t, "h_bucket{le=\"0\"} 5\nh_bucket{le=\"+Inf\"} 5\n")
	v, ok = Quantile(0.5, nil, neg, "h", nil)
	require.True(t, ok)
	require.Equal(t, 0.0, v)

	// unsorted input and a malformed le are tolerated / rejected
	shuffled := mustParse(t, "h_bucket{le=\"+Inf\"} 10\nh_bucket{le=\"0.5\"} 10\nh_bucket{le=\"0.1\"} 0\n")
	v, ok = Quantile(0.5, nil, shuffled, "h", nil)
	require.True(t, ok)
	require.InDelta(t, 0.1+0.4*0.5, v, 1e-9)
	_, ok = Quantile(0.5, nil, mustParse(t, "h_bucket{le=\"x\"} 1\nh_bucket{le=\"+Inf\"} 1\n"), "h", nil)
	require.False(t, ok)
	_, ok = Quantile(0.5, nil, mustParse(t, "h_bucket 1\n"), "h", nil)
	require.False(t, ok, "bucket without le")
}
