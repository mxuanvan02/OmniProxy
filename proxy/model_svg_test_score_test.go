package proxy

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// svgScoreFixture builds a well-formed drawing with a controllable shape count
// and one <animate> per named attribute, so each rubric term can be varied
// without disturbing the others.
func svgScoreFixture(shapes int, attrs ...string) string {
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 800 450">`)
	for i := 0; i < shapes; i++ {
		b.WriteString(`<circle cx="10" cy="10" r="4"/>`)
	}
	for _, a := range attrs {
		b.WriteString(`<animate attributeName="` + a + `" dur="2s" repeatCount="indefinite"/>`)
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func TestScoreSVG(t *testing.T) {
	tests := []struct {
		name        string
		svg         string
		wantScore   int
		wantReasons []string
	}{
		{
			name:        "empty input scores zero",
			svg:         "",
			wantScore:   0,
			wantReasons: []string{"no svg"},
		},
		{
			name:        "whitespace only scores zero",
			svg:         "   \n\t ",
			wantScore:   0,
			wantReasons: []string{"no svg"},
		},
		{
			name:        "rich two-motion drawing",
			svg:         svgScoreFixture(60, "cx", "opacity"),
			wantScore:   89,
			wantReasons: nil,
		},
		{
			name:        "single animated attribute loses to two",
			svg:         svgScoreFixture(60, "cx"),
			wantScore:   82,
			wantReasons: []string{"only one animated attribute"},
		},
		{
			name:        "sparse drawing",
			svg:         svgScoreFixture(5, "cx", "opacity"),
			wantScore:   71,
			wantReasons: []string{"sparse: 5 shapes"},
		},
		{
			name:        "unclosed root is malformed",
			svg:         `<svg viewBox="0 0 800 450"><rect/>`,
			wantScore:   28,
			wantReasons: []string{"malformed XML", "no SMIL animation", "sparse: 1 shapes"},
		},
		{
			name:        "external resource reference is penalised",
			svg:         `<svg viewBox="0 0 800 450"><image href="https://cdn.example.com/a.png"/><animate attributeName="cx"/></svg>`,
			wantScore:   47,
			wantReasons: []string{"external resource reference", "only one animated attribute", "sparse: 0 shapes"},
		},
		{
			name:        "missing viewBox",
			svg:         `<svg xmlns="http://www.w3.org/2000/svg"><animate attributeName="cx"/><animate attributeName="r"/></svg>`,
			wantScore:   49,
			wantReasons: []string{"no viewBox", "sparse: 0 shapes"},
		},
		{
			name:        "inverted aspect ratio gets partial viewBox credit",
			svg:         `<svg viewBox="0 0 100 1000"><animate attributeName="cx"/><animate attributeName="r"/></svg>`,
			wantScore:   59,
			wantReasons: []string{"sparse: 0 shapes", "unusual viewBox aspect 0.10"},
		},
		{
			name:        "truncated viewBox is malformed",
			svg:         `<svg viewBox="0 0 800"><animate attributeName="cx"/><animate attributeName="r"/></svg>`,
			wantScore:   59,
			wantReasons: []string{"malformed viewBox", "sparse: 0 shapes"},
		},
		{
			name:        "zero viewBox size",
			svg:         `<svg viewBox="0 0 0 450"><animate attributeName="cx"/><animate attributeName="r"/></svg>`,
			wantScore:   59,
			wantReasons: []string{"non-positive viewBox size", "sparse: 0 shapes"},
		},
		{
			name:        "animation without attributeName",
			svg:         `<svg viewBox="0 0 800 450"><animate dur="2s"/><animate dur="3s"/></svg>`,
			wantScore:   54,
			wantReasons: []string{"animation sets no attributeName", "sparse: 0 shapes"},
		},
		{
			name:        "static drawing has no motion",
			svg:         svgScoreFixture(60),
			wantScore:   75,
			wantReasons: []string{"no SMIL animation"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotScore, gotReasons := scoreSVG(tt.svg)
			if gotScore != tt.wantScore {
				t.Errorf("scoreSVG(%q) score = %d, want %d (reasons %v)", tt.svg, gotScore, tt.wantScore, gotReasons)
			}
			if !reflect.DeepEqual(normalizeReasons(gotReasons), normalizeReasons(tt.wantReasons)) {
				t.Errorf("scoreSVG(%q) reasons = %v, want %v", tt.svg, gotReasons, tt.wantReasons)
			}
			if gotScore < 0 || gotScore > 100 {
				t.Errorf("scoreSVG(%q) score %d outside 0-100", tt.svg, gotScore)
			}
		})
	}
}

// normalizeReasons treats nil and empty as the same so a table entry can write
// nil for "no complaints".
func normalizeReasons(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return in
}

// TestScoreSVGRewardsDistinctMotionAndDensity guards the two comparisons the
// rubric exists to make: more independently animated properties and a denser
// drawing must both score strictly higher.
func TestScoreSVGRewardsDistinctMotionAndDensity(t *testing.T) {
	twoAttrs, _ := scoreSVG(svgScoreFixture(60, "cx", "opacity"))
	oneAttr, _ := scoreSVG(svgScoreFixture(60, "cx"))
	repeatedAttr, _ := scoreSVG(svgScoreFixture(60, "cx", "cx"))
	if !(twoAttrs > oneAttr) {
		t.Errorf("two distinct animated attributes = %d, want > one attribute = %d", twoAttrs, oneAttr)
	}
	if !(twoAttrs > repeatedAttr) {
		t.Errorf("two distinct attributes = %d, want > the same attribute twice = %d", twoAttrs, repeatedAttr)
	}

	dense, _ := scoreSVG(svgScoreFixture(60, "cx", "opacity"))
	sparse, _ := scoreSVG(svgScoreFixture(5, "cx", "opacity"))
	if !(dense > sparse) {
		t.Errorf("60 shapes = %d, want > 5 shapes = %d", dense, sparse)
	}
}

// TestScoreSVGDoesNotSaturate is the regression that sent the first rubric back
// for reweighting: a batch of plausible results scored 8/14 at exactly 100,
// which makes the column useless for comparing models. Any reweighting that
// collapses these back onto one value must fail here.
func TestScoreSVGDoesNotSaturate(t *testing.T) {
	corpus := []string{
		svgScoreFixture(0),
		svgScoreFixture(5, "cx"),
		svgScoreFixture(20, "cx"),
		svgScoreFixture(20, "cx", "opacity"),
		svgScoreFixture(60, "cx"),
		svgScoreFixture(60, "cx", "opacity"),
		svgScoreFixture(60, "cx", "opacity", "r", "fill"),
		`<svg viewBox="0 0 100 1000"><animate attributeName="cx"/></svg>`,
		`<svg viewBox="0 0 800 450"><image href="//cdn.example.com/x.png"/><animate attributeName="cx"/><animate attributeName="r"/></svg>`,
		strings.Replace(svgScoreFixture(60, "cx", "opacity"), "</svg>", "", 1),
	}

	distinct := map[int]bool{}
	for _, svg := range corpus {
		score, _ := scoreSVG(svg)
		distinct[score] = true
	}
	if len(distinct) <= 5 {
		t.Errorf("rubric collapsed %d results into %d distinct scores (<=5), it no longer discriminates", len(corpus), len(distinct))
	}
	if distinct[100] {
		t.Errorf("a fixture scored the maximum 100; the corpus should leave headroom so real results can still be separated")
	}
}

func TestSVGXMLWellFormed(t *testing.T) {
	tests := []struct {
		svg  string
		want bool
	}{
		{`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><circle/></svg>`, true},
		{`<svg><g><rect/></g></svg>`, true},
		{`<svg><rect/></g></svg>`, false},
		{`<svg><rect/>`, false},
		{`<svg><rect width="1"></svg>`, false},
		{``, false},
	}
	for _, tt := range tests {
		if got := svgXMLWellFormed(tt.svg); got != tt.want {
			t.Errorf("svgXMLWellFormed(%q) = %v, want %v", tt.svg, got, tt.want)
		}
	}
}

func TestSVGScoreViewBox(t *testing.T) {
	tests := []struct {
		name       string
		svg        string
		wantPoints int
		wantReason string
	}{
		{"16:9 is full credit", `<svg viewBox="0 0 800 450"/>`, svgScoreViewBoxPoints, ""},
		{"square is full credit", `<svg viewBox="0 0 512 512"/>`, svgScoreViewBoxPoints, ""},
		{"case and commas", `<svg VIEWBOX="0,0,800,450"/>`, svgScoreViewBoxPoints, ""},
		{"absent", `<svg width="800"/>`, 0, "no viewBox"},
		{"two numbers only", `<svg viewBox="800 450"/>`, svgScoreViewBoxOddRatio, "malformed viewBox"},
		{"negative height", `<svg viewBox="0 0 800 -450"/>`, svgScoreViewBoxOddRatio, "non-positive viewBox size"},
		{"non-numeric", `<svg viewBox="0 0 wide tall"/>`, svgScoreViewBoxOddRatio, "non-positive viewBox size"},
		{"too wide", `<svg viewBox="0 0 3000 450"/>`, svgScoreViewBoxOddRatio, "unusual viewBox aspect 6.67"},
		{"too tall", `<svg viewBox="0 0 100 1000"/>`, svgScoreViewBoxOddRatio, "unusual viewBox aspect 0.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			points, reason := svgScoreViewBox(tt.svg)
			if points != tt.wantPoints || reason != tt.wantReason {
				t.Errorf("svgScoreViewBox(%q) = (%d, %q), want (%d, %q)", tt.svg, points, reason, tt.wantPoints, tt.wantReason)
			}
		})
	}
}

func TestSVGDistinctAnimatedAttrs(t *testing.T) {
	tests := []struct {
		svg  string
		want []string
	}{
		{`<animate attributeName="cx"/><animate attributeName="opacity"/>`, []string{"cx", "opacity"}},
		{`<animate attributeName="cx"/><animate attributeName="CX"/>`, []string{"cx"}},
		{`<animate attributeName="cx"/><animate attributeName="cx"/>`, []string{"cx"}},
		{`<animate attributeName=""/><animate attributeName="  "/>`, nil},
		{`<circle cx="1"/>`, nil},
		{`<animateTransform attributeName="transform"/><set attributeName="fill"/>`, []string{"fill", "transform"}},
	}
	for _, tt := range tests {
		got := svgDistinctAnimatedAttrs(tt.svg)
		if !reflect.DeepEqual(normalizeReasons(got), normalizeReasons(tt.want)) {
			t.Errorf("svgDistinctAnimatedAttrs(%q) = %v, want %v", tt.svg, got, tt.want)
		}
	}
}

func TestSVGScoreRichness(t *testing.T) {
	tests := []struct {
		shapes int
		want   int
	}{
		{-1, 0},
		{0, 0},
		{1, 3},
		{5, 7},
		{15, 13},
		{60, svgScoreRichnessMax},
		{600, svgScoreRichnessMax},
	}
	for _, tt := range tests {
		if got := svgScoreRichness(tt.shapes); got != tt.want {
			t.Errorf("svgScoreRichness(%d) = %d, want %d", tt.shapes, got, tt.want)
		}
	}
}

func TestSVGTestEfficiency(t *testing.T) {
	tests := []struct {
		name      string
		score     int
		tokens    int
		wantValue float64
		wantOK    bool
	}{
		{"score per thousand tokens", 50, 2000, 25.0, true},
		{"perfect score at a thousand tokens", 100, 1000, 100.0, true},
		{"expensive run scores low", 98, 20151, 98.0 / 20151 * 1000, true},
		{"gateway reported no usage", 89, 0, 0, false},
		{"negative token count", 89, -12, 0, false},
		{"failed run has no score", 0, 5000, 0, false},
		{"negative score", -3, 5000, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := svgTestEfficiency(tt.score, tt.tokens)
			if ok != tt.wantOK {
				t.Fatalf("svgTestEfficiency(%d, %d) ok = %v, want %v", tt.score, tt.tokens, ok, tt.wantOK)
			}
			if math.Abs(got-tt.wantValue) > 1e-9 {
				t.Errorf("svgTestEfficiency(%d, %d) = %v, want %v", tt.score, tt.tokens, got, tt.wantValue)
			}
			if ok && (math.IsInf(got, 0) || math.IsNaN(got)) {
				t.Errorf("svgTestEfficiency(%d, %d) = %v, must never be infinite or NaN", tt.score, tt.tokens, got)
			}
		})
	}
}
