package proxy

import "testing"

func TestClassifySVGReply(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantSVG    bool
		wantReason string
	}{
		{
			name:       "complete SVG succeeds with no reason",
			input:      `<svg viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"/></svg>`,
			wantSVG:    true,
			wantReason: "",
		},
		{
			name:    "truncated SVG is reported as cut off, not as no-SVG",
			input:   `<svg viewBox="0 0 900 600"><path d="M0 0" dur="0.9s" repeatCount`,
			wantSVG: false,
			// The whole point: a token-cap truncation must not read as a refusal.
			wantReason: "SVG was cut off before </svg> — output likely hit the token cap",
		},
		{
			name:       "prose with no markup",
			input:      "I cannot generate SVG images.",
			wantSVG:    false,
			wantReason: "reply contained no SVG markup",
		},
		{
			name:       "empty reply",
			input:      "   ",
			wantSVG:    false,
			wantReason: "empty reply from model",
		},
		{
			name:       "fenced complete SVG succeeds",
			input:      "```svg\n<svg viewBox=\"0 0 4 4\"><rect width=\"4\" height=\"4\"/></svg>\n```",
			wantSVG:    true,
			wantReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svg, reason := classifySVGReply(tt.input)
			if (svg != "") != tt.wantSVG {
				t.Errorf("classifySVGReply() svg present = %v, want %v (svg=%q)", svg != "", tt.wantSVG, svg)
			}
			if reason != tt.wantReason {
				t.Errorf("classifySVGReply() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}
