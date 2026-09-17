package proxy

import (
	"encoding/xml"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Deterministic quality scoring for the SVG model test.
//
// The test asks for a self-contained animated SVG with a viewBox and SMIL
// motion, so every requirement in the prompt is checkable without asking a
// model to judge the result — which would spend tokens and add a second source
// of noise to a comparison that exists to remove noise.
//
// The rubric is weighted so it discriminates. An earlier draft that simply
// asked "does it have a viewBox / an <animate> / no external refs / parseable
// XML" scored 8 of the 14 stored results at a perfect 100, which ranks nothing.
// The band that actually separates results is how many *distinct* properties a
// model animates — the prompt names two separate motions (wheels spinning, body
// bobbing) — and how much structure it draws to carry them. Those two checks
// hold most of the points.
//
// Scores are computed on read (see readSVGTestEntry) rather than persisted, so
// results stored before scoring existed are scored too, and a rubric change
// cannot leave stale scores disagreeing with the current one.

const (
	svgScoreXMLPoints       = 25
	svgScoreViewBoxPoints   = 15
	svgScoreViewBoxOddRatio = 10
	svgScoreMotionMax       = 15
	svgScoreDistinctMax     = 10
	svgScoreNoExternalPts   = 10
	svgScoreRichnessMax     = 25
	// svgScoreRichnessFullShapes is the shape count at which richness stops
	// adding points. Square-root scaled below it, so a sparse drawing and a
	// merely-different one do not score alike.
	svgScoreRichnessFullShapes = 60
)

var (
	svgViewBoxPattern   = regexp.MustCompile(`(?i)viewbox\s*=\s*"([^"]*)"`)
	svgMotionPattern    = regexp.MustCompile(`(?i)<(animate|animatetransform|animatemotion|set)[\s>/]`)
	svgAttrNamePattern  = regexp.MustCompile(`(?i)attributeName\s*=\s*"([^"]*)"`)
	svgShapePattern     = regexp.MustCompile(`(?i)<(circle|rect|ellipse|path|polygon|polyline|line)[\s>/]`)
	svgExternalPattern  = regexp.MustCompile(`(?i)(xlink:href|href|src)\s*=\s*["']\s*(https?:|//)`)
	svgViewBoxSepChars  = ", \t\n\r"
	svgScoreAspectLower = 1.0
	svgScoreAspectUpper = 2.0
)

// scoreSVG grades one stored SVG against the prompt's stated requirements and
// returns the 0-100 score plus the checks it lost points on. The reasons exist
// so a low score is explainable to the operator instead of opaque.
func scoreSVG(svg string) (int, []string) {
	if strings.TrimSpace(svg) == "" {
		return 0, []string{"no svg"}
	}

	score := 0
	var reasons []string

	if svgXMLWellFormed(svg) {
		score += svgScoreXMLPoints
	} else {
		reasons = append(reasons, "malformed XML")
	}

	points, reason := svgScoreViewBox(svg)
	score += points
	if reason != "" {
		reasons = append(reasons, reason)
	}

	motion := svgMotionPattern.FindAllString(svg, -1)
	if len(motion) == 0 {
		reasons = append(reasons, "no SMIL animation")
	} else {
		score += min(svgScoreMotionMax, len(motion)*2)
	}

	attrs := svgDistinctAnimatedAttrs(svg)
	if len(attrs) == 0 && len(motion) > 0 {
		reasons = append(reasons, "animation sets no attributeName")
	} else {
		score += min(svgScoreDistinctMax, len(attrs)*5)
		if len(attrs) == 1 {
			reasons = append(reasons, "only one animated attribute")
		}
	}

	if svgExternalPattern.MatchString(svg) {
		reasons = append(reasons, "external resource reference")
	} else {
		score += svgScoreNoExternalPts
	}

	shapes := len(svgShapePattern.FindAllString(svg, -1))
	score += svgScoreRichness(shapes)
	if shapes < 20 {
		reasons = append(reasons, "sparse: "+strconv.Itoa(shapes)+" shapes")
	}

	if score > 100 {
		score = 100
	}
	sort.Strings(reasons)
	return score, reasons
}

// svgXMLWellFormed reports whether the markup parses as XML. A truncated or
// tag-unbalanced SVG may still render in a forgiving browser, but it is not the
// valid markup the prompt asked for. An empty document is rejected: well-formed
// XML needs a root element, and the decoder reports EOF on nothing at all.
func svgXMLWellFormed(svg string) bool {
	dec := xml.NewDecoder(strings.NewReader(svg))
	sawRoot := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return err == io.EOF && sawRoot
		}
		if _, ok := tok.(xml.StartElement); ok {
			sawRoot = true
		}
	}
}

// svgScoreViewBox awards the viewBox points, at the reduced rate when the box
// parses but has a nonsensical aspect ratio (a zero or inverted box scales the
// drawing away entirely, so it is not full credit).
func svgScoreViewBox(svg string) (int, string) {
	m := svgViewBoxPattern.FindStringSubmatch(svg)
	if m == nil {
		return 0, "no viewBox"
	}
	parts := strings.FieldsFunc(m[1], func(r rune) bool {
		return strings.IndexRune(svgViewBoxSepChars, r) >= 0
	})
	if len(parts) != 4 {
		return svgScoreViewBoxOddRatio, "malformed viewBox"
	}
	w, errW := strconv.ParseFloat(parts[2], 64)
	h, errH := strconv.ParseFloat(parts[3], 64)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return svgScoreViewBoxOddRatio, "non-positive viewBox size"
	}
	ratio := w / h
	if ratio < svgScoreAspectLower || ratio > svgScoreAspectUpper {
		return svgScoreViewBoxOddRatio, "unusual viewBox aspect " + strconv.FormatFloat(ratio, 'f', 2, 64)
	}
	return svgScoreViewBoxPoints, ""
}

// svgDistinctAnimatedAttrs collects the unique properties the markup animates.
// Counting distinct values rather than element occurrences is what rewards the
// two independent motions the prompt asks for over one motion repeated.
func svgDistinctAnimatedAttrs(svg string) []string {
	seen := map[string]bool{}
	for _, m := range svgAttrNamePattern.FindAllStringSubmatch(svg, -1) {
		if name := strings.ToLower(strings.TrimSpace(m[1])); name != "" {
			seen[name] = true
		}
	}
	attrs := make([]string, 0, len(seen))
	for name := range seen {
		attrs = append(attrs, name)
	}
	sort.Strings(attrs)
	return attrs
}

// svgScoreRichness maps a shape count onto the richness points, square-root
// scaled so the first few shapes count for more than the last few.
func svgScoreRichness(shapes int) int {
	if shapes <= 0 {
		return 0
	}
	fraction := float64(shapes) / float64(svgScoreRichnessFullShapes)
	if fraction > 1 {
		fraction = 1
	}
	return int(math.Round(float64(svgScoreRichnessMax) * math.Sqrt(fraction)))
}

// svgTestEfficiency converts a score and a token count into score per thousand
// tokens — the measure that answers "which model bought the most quality for
// its spend" when the prompt is identical across runs. It returns ok=false when
// the spend is unknown: some chat gateways ignore stream_options.include_usage
// and report no usage at all, and those runs must show "n/a" rather than a
// division by zero or a fabricated figure.
func svgTestEfficiency(score, tokens int) (float64, bool) {
	if score <= 0 || tokens <= 0 {
		return 0, false
	}
	return float64(score) / float64(tokens) * 1000, true
}
