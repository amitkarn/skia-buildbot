package parser

import (
	"math"
	"testing"

	"skia.googlesource.com/buildbot.git/perf/go/config"
	"skia.googlesource.com/buildbot.git/perf/go/types"
)

func newTestContext() *Context {
	tile := types.NewTile()
	t1 := types.NewTraceN(3)
	t1.Params["os"] = "Ubuntu12"
	t1.Params["config"] = "8888"
	t1.Values[1] = 1.234
	tile.Traces["t1"] = t1

	t2 := types.NewTraceN(3)
	t2.Params["os"] = "Ubuntu12"
	t2.Params["config"] = "gpu"
	t2.Values[1] = 1.236
	tile.Traces["t2"] = t2

	return NewContext(tile)
}

func TestFilter(t *testing.T) {
	ctx := newTestContext()

	testCases := []struct {
		input  string
		length int
	}{
		{`filter("os=Ubuntu12")`, 2},
		{`filter("")`, 2},
		{`filter("config=8888")`, 1},
		{`filter("config=gpu")`, 1},
		{`filter("config=565")`, 0},
	}
	for _, tc := range testCases {
		traces, err := ctx.Eval(tc.input)
		if err != nil {
			t.Fatalf("Failed to run filter %q: %s", tc.input, err)
		}
		if got, want := len(traces), tc.length; got != want {
			t.Errorf("Wrong traces length %q: Got %v Want %v", tc.input, got, want)
		}
	}
}

func TestEvalNoModifyTile(t *testing.T) {
	ctx := newTestContext()

	traces, err := ctx.Eval(`fill(filter("config=8888"))`)
	if err != nil {
		t.Fatalf("Failed to run filter: %s", err)
	}
	// Make sure we made a deep copy of the traces in the Tile.
	if got, want := ctx.Tile.Traces["t1"].Values[0], config.MISSING_DATA_SENTINEL; got != want {
		t.Errorf("Tile incorrectly modified: Got %v Want %v", got, want)
	}
	if got, want := traces[0].Values[0], 1.234; got != want {
		t.Errorf("fill() failed: Got %v Want %v", got, want)
	}
}

func TestEvalErrors(t *testing.T) {
	ctx := newTestContext()

	testCases := []string{
		// Invalid forms.
		`filter("os=Ubuntu12"`,
		`filter(")`,
		`"config=8888"`,
		`("config=gpu")`,
		`{}`,
		// Forms that fail internal Func validation.
		`filter(2)`,
		`norm("foo")`,
		`norm(filter(""), "foo")`,
		`ave(2)`,
		`norm(2)`,
		`fill(2)`,
		`norm()`,
		`ave()`,
		`fill()`,
	}
	for _, tc := range testCases {
		_, err := ctx.Eval(tc)
		if err == nil {
			t.Fatalf("Expected %q to fail parsing:", tc)
		}
	}
}

func near(a, b float64) bool {
	return math.Abs(a-b) < 0.001
}

func TestNorm(t *testing.T) {
	ctx := newTestContext()
	ctx.Tile.Traces["t1"].Values = []float64{2.0, -2.0, 1e100}
	delete(ctx.Tile.Traces, "t2")
	traces, err := ctx.Eval(`norm(filter(""))`)
	if err != nil {
		t.Fatalf("Failed to eval norm() test: %s", err)
	}

	if got, want := traces[0].Values[0], 1.0; !near(got, want) {
		t.Errorf("Distance mismatch: Got %v Want %v Full %#v", got, want, traces[0].Values)
	}
}

func TestAve(t *testing.T) {
	ctx := newTestContext()
	ctx.Tile.Traces["t1"].Values = []float64{1.0, -1.0, 1e100, 1e100}
	ctx.Tile.Traces["t2"].Values = []float64{1e100, 2.0, -2.0, 1e100}
	traces, err := ctx.Eval(`ave(filter(""))`)
	if err != nil {
		t.Fatalf("Failed to eval ave() test: %s", err)
	}
	if got, want := len(traces), 1; got != want {
		t.Errorf("ave() returned wrong length: Got %v Want %v", got, want)
	}

	for i, want := range []float64{1.0, 0.5, -2.0, 1e100} {
		if got := traces[0].Values[i]; !near(got, want) {
			t.Errorf("Distance mismatch: Got %v Want %v", got, want)
		}
	}
}

func TestFill(t *testing.T) {
	ctx := newTestContext()
	ctx.Tile.Traces["t1"].Values = []float64{1e100, 1e100, 2, 3, 1e100, 5}
	delete(ctx.Tile.Traces, "t2")
	traces, err := ctx.Eval(`fill(filter("config=8888"))`)
	if err != nil {
		t.Fatalf("Failed to eval fill() test: %s", err)
	}
	if got, want := len(traces), 1; got != want {
		t.Errorf("fill() returned wrong length: Got %v Want %v", got, want)
	}

	for i, want := range []float64{2, 2, 2, 3, 3, 5} {
		if got := traces[0].Values[i]; !near(got, want) {
			t.Errorf("Distance mismatch: Got %v Want %v", got, want)
		}
	}
}
