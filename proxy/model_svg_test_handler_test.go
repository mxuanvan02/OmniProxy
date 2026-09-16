package proxy

import "testing"

func TestExtractSVG(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain SVG",
			input: `<svg viewBox="0 0 100 100"><circle cx="50" cy="50" r="40"/></svg>`,
			want:  `<svg viewBox="0 0 100 100"><circle cx="50" cy="50" r="40"/></svg>`,
		},
		{
			name:  "SVG in svg markdown fence",
			input: "```svg\n<svg viewBox=\"0 0 100 100\"><rect width=\"100\" height=\"100\"/></svg>\n```",
			want:  `<svg viewBox="0 0 100 100"><rect width="100" height="100"/></svg>`,
		},
		{
			name:  "SVG in generic markdown fence",
			input: "```\n<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 50 50\"><path d=\"M0 0\"/></svg>\n```",
			want:  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 50 50"><path d="M0 0"/></svg>`,
		},
		{
			name:  "SVG with preamble text",
			input: "Here is the SVG:\n<svg viewBox=\"0 0 200 200\"><text x=\"10\" y=\"20\">hi</text></svg>\nHope that helps!",
			want:  `<svg viewBox="0 0 200 200"><text x="10" y="20">hi</text></svg>`,
		},
		{
			name:  "no SVG at all",
			input: "I cannot generate SVG images. Please try another model.",
			want:  "",
		},
		{
			name:  "opening tag but no closing",
			input: `<svg viewBox="0 0 100 100"><circle cx="50" cy="50" r="40"/>`,
			want:  "",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "SVG with xml fence",
			input: "```xml\n<svg viewBox=\"0 0 10 10\"><line x1=\"0\" y1=\"0\" x2=\"10\" y2=\"10\"/></svg>\n```",
			want:  `<svg viewBox="0 0 10 10"><line x1="0" y1="0" x2="10" y2="10"/></svg>`,
		},
		{
			name:  "multiple SVGs takes first-to-last",
			input: `<svg id="a"><rect/></svg> some text <svg id="b"><circle/></svg>`,
			want:  `<svg id="a"><rect/></svg> some text <svg id="b"><circle/></svg>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSVG(tt.input)
			if got != tt.want {
				t.Errorf("extractSVG() = %q, want %q", got, tt.want)
			}
		})
	}
}
