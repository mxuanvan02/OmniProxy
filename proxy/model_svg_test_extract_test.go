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

func TestClassifySVGReplyWithStopReason(t *testing.T) {
	truncated := `<svg viewBox="0 0 900 600"><path d="M0 0" dur="0.9s" repeatCount`
	tests := []struct {
		name       string
		stop       string
		wantReason string
	}{
		{
			name:       "token-cap stop reason keeps the cap message",
			stop:       "max_tokens",
			wantReason: "SVG was cut off before </svg> — output likely hit the token cap",
		},
		{
			name:       "length stop reason keeps the cap message",
			stop:       "length",
			wantReason: "SVG was cut off before </svg> — output likely hit the token cap",
		},
		{
			name:       "early stream end is reported, not blamed on the cap",
			stop:       "end_turn",
			wantReason: "SVG was cut off before </svg> — stream ended early (stop_reason=end_turn)",
		},
		{
			name:       "unknown stop reason falls back to the cap message",
			stop:       "",
			wantReason: "SVG was cut off before </svg> — output likely hit the token cap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svg, reason := classifySVGReplyWithStop(truncated, tt.stop)
			if svg != "" {
				t.Errorf("classifySVGReplyWithStop() svg = %q, want empty", svg)
			}
			if reason != tt.wantReason {
				t.Errorf("classifySVGReplyWithStop() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}

	t.Run("complete SVG ignores the stop reason", func(t *testing.T) {
		svg, reason := classifySVGReplyWithStop(`<svg viewBox="0 0 4 4"><rect width="4" height="4"/></svg>`, "end_turn")
		if svg == "" || reason != "" {
			t.Errorf("classifySVGReplyWithStop() svg=%q reason=%q, want svg and no reason", svg, reason)
		}
	})
}
