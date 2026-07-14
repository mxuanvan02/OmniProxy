package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LineType represents the type of a TOML line
type LineType int

const (
	LineBlank LineType = iota
	LineComment
	LineKeyValue
	LineSection
)

// ConfigLine represents a parsed TOML line
type ConfigLine struct {
	Raw      string
	Type     LineType
	Key      string
	Value    string
	IsActive bool
	Section  string
}

// MergeState tracks the configuration state during merging
type MergeState struct {
	ActiveModel         string
	ActiveProvider      string
	SubagentModel       string
	HasSuperKiroSection bool
	HasSubagentSection  bool
	SuperKiroSectionEnd int
	SubagentSectionEnd  int
	Lines               []ConfigLine
}

// parseTomlValue strips an inline comment and matching surrounding quotes from
// a TOML value. A # inside a basic or literal string is data, not a comment.
func parseTomlValue(valuePart string) string {
	valuePart = strings.TrimSpace(valuePart)
	quote := byte(0)
	escaped := false
	for i := 0; i < len(valuePart); i++ {
		ch := valuePart[i]
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'':
			quote = ch
		case '#':
			valuePart = strings.TrimSpace(valuePart[:i])
			i = len(valuePart)
		}
	}
	if len(valuePart) >= 2 {
		first, last := valuePart[0], valuePart[len(valuePart)-1]
		if (first == '"' || first == '\'') && last == first {
			valuePart = valuePart[1 : len(valuePart)-1]
		}
	}
	return valuePart
}

// parseTomlLine parses a single TOML line
func parseTomlLine(line string, currentSection string) ConfigLine {
	cl := ConfigLine{
		Raw:     line,
		Section: currentSection,
	}

	trimmed := strings.TrimSpace(line)

	// Blank line
	if trimmed == "" {
		cl.Type = LineBlank
		cl.IsActive = false
		return cl
	}

	// Comment line
	if strings.HasPrefix(trimmed, "#") {
		cl.Type = LineComment
		cl.IsActive = false
		// Try to parse as key-value even if commented for tracking
		content := strings.TrimPrefix(trimmed, "#")
		content = strings.TrimSpace(content)
		if idx := strings.Index(content, "="); idx > 0 {
			cl.Key = strings.TrimSpace(content[:idx])
			cl.Value = parseTomlValue(content[idx+1:])
		}
		return cl
	}

	// Section header
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		cl.Type = LineSection
		cl.IsActive = true
		cl.Key = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		return cl
	}

	// Key-value pair
	if idx := strings.Index(trimmed, "="); idx > 0 {
		cl.Type = LineKeyValue
		cl.IsActive = true
		cl.Key = strings.TrimSpace(trimmed[:idx])

		cl.Value = parseTomlValue(trimmed[idx+1:])
		return cl
	}

	// Unknown type, treat as comment
	cl.Type = LineComment
	cl.IsActive = false
	return cl
}

// scanConfig builds state from existing config lines
func scanConfig(lines []string) MergeState {
	state := MergeState{
		Lines: make([]ConfigLine, 0, len(lines)),
	}

	currentSection := ""
	inSuperKiro := false
	inSubagent := false

	for i, line := range lines {
		cl := parseTomlLine(line, currentSection)

		if cl.Type == LineSection {
			currentSection = cl.Key

			if cl.Key == "model_providers.superkiro" {
				state.HasSuperKiroSection = true
				inSuperKiro = true
				inSubagent = false
			} else if cl.Key == "agents.subagent" {
				state.HasSubagentSection = true
				inSubagent = true
				inSuperKiro = false
			} else {
				if inSuperKiro {
					state.SuperKiroSectionEnd = i
				}
				if inSubagent {
					state.SubagentSectionEnd = i
				}
				inSuperKiro = false
				inSubagent = false
			}
		} else if cl.Type == LineKeyValue && cl.IsActive {
			if currentSection == "" {
				// Top-level settings
				if cl.Key == "model" {
					state.ActiveModel = cl.Value
				} else if cl.Key == "model_provider" {
					state.ActiveProvider = cl.Value
				}
			} else if currentSection == "agents.subagent" {
				if cl.Key == "model" {
					state.SubagentModel = cl.Value
				}
			}
		}

		state.Lines = append(state.Lines, cl)
	}

	// Handle case where section extends to EOF
	if inSuperKiro {
		state.SuperKiroSectionEnd = len(lines)
	}
	if inSubagent {
		state.SubagentSectionEnd = len(lines)
	}

	return state
}

// MergeCodexConfig merges SuperKiro configuration into existing Codex config.toml
func MergeCodexConfig(homeDir, model, baseURL, subagent string) error {
	configPath := filepath.Join(homeDir, ".codex", "config.toml")

	// Read existing config or create empty
	var existingLines []string
	data, err := os.ReadFile(configPath)
	if err == nil {
		existingLines = strings.Split(string(data), "\n")
	} else {
		existingLines = []string{}
	}

	// Scan existing config
	state := scanConfig(existingLines)

	// Process lines
	var output []string
	currentSection := ""
	skipUntilNextSection := false

	modelInjected := false
	providerInjected := false
	superKiroInjected := false
	subagentInjected := false

	for i, cl := range state.Lines {
		if cl.Type == LineSection {
			currentSection = cl.Key

			if cl.Key == "model_providers.superkiro" {
				skipUntilNextSection = false

				// Inject SuperKiro section
				output = append(output, "[model_providers.superkiro]")
				output = append(output, fmt.Sprintf(`name = "SuperKiro"`))
				output = append(output, fmt.Sprintf(`base_url = "%s"`, baseURL))
				output = append(output, `wire_api = "responses"`)
				superKiroInjected = true
				skipUntilNextSection = true
				continue
			} else if cl.Key == "agents.subagent" {
				skipUntilNextSection = false

				// Inject subagent section
				output = append(output, "[agents.subagent]")
				output = append(output, fmt.Sprintf(`model = "%s"`, subagent))
				subagentInjected = true
				skipUntilNextSection = true
				continue
			} else {
				skipUntilNextSection = false
				output = append(output, cl.Raw)
			}
		} else if skipUntilNextSection {
			// Skip lines inside replaced sections
			continue
		} else if cl.Type == LineKeyValue && cl.IsActive && currentSection == "" {
			// Top-level key-value
			if cl.Key == "model" {
				if cl.Value != model {
					// Comment out old value
					output = append(output, "#"+cl.Raw)
					if !modelInjected {
						output = append(output, fmt.Sprintf(`model = "%s"`, model))
						modelInjected = true
					}
				} else {
					// Same value, keep as-is
					output = append(output, cl.Raw)
					modelInjected = true
				}
			} else if cl.Key == "model_provider" {
				if cl.Value != "superkiro" {
					// Comment out old value
					output = append(output, "#"+cl.Raw)
					if !providerInjected {
						output = append(output, `model_provider = "superkiro"`)
						providerInjected = true
					}
				} else {
					// Same value, keep as-is
					output = append(output, cl.Raw)
					providerInjected = true
				}
			} else {
				// Other top-level settings, keep as-is
				output = append(output, cl.Raw)
			}
		} else {
			// All other lines (comments, blanks, other sections)
			output = append(output, cl.Raw)
		}

		// Inject missing top-level settings after first non-comment line
		if i == 0 && cl.Type != LineComment && cl.Type != LineBlank {
			if !modelInjected && state.ActiveModel == "" {
				output = append(output, fmt.Sprintf(`model = "%s"`, model))
				modelInjected = true
			}
			if !providerInjected && state.ActiveProvider == "" {
				output = append(output, `model_provider = "superkiro"`)
				providerInjected = true
			}
		}
	}

	// If empty config or missing sections, inject at appropriate places
	if len(existingLines) == 0 {
		output = []string{
			fmt.Sprintf(`# SuperKiro Configuration for Codex CLI`),
			fmt.Sprintf(`model = "%s"`, model),
			`model_provider = "superkiro"`,
			"",
			"[model_providers.superkiro]",
			`name = "SuperKiro"`,
			fmt.Sprintf(`base_url = "%s"`, baseURL),
			`wire_api = "responses"`,
			"",
			"[agents.subagent]",
			fmt.Sprintf(`model = "%s"`, subagent),
		}
	} else {
		// Inject missing top-level settings at the start if not yet done
		if !modelInjected || !providerInjected {
			header := []string{}
			if !modelInjected {
				header = append(header, fmt.Sprintf(`model = "%s"`, model))
			}
			if !providerInjected {
				header = append(header, `model_provider = "superkiro"`)
			}
			header = append(header, "")
			output = append(header, output...)
		}

		// Inject missing sections at the end
		if !superKiroInjected {
			output = append(output, "")
			output = append(output, "[model_providers.superkiro]")
			output = append(output, `name = "SuperKiro"`)
			output = append(output, fmt.Sprintf(`base_url = "%s"`, baseURL))
			output = append(output, `wire_api = "responses"`)
		}

		if !subagentInjected {
			output = append(output, "")
			output = append(output, "[agents.subagent]")
			output = append(output, fmt.Sprintf(`model = "%s"`, subagent))
		}
	}

	// Write back
	content := strings.Join(output, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	return os.WriteFile(configPath, []byte(content), 0644)
}
