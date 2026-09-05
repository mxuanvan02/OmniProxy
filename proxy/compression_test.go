package proxy

import (
	"strings"
	"testing"
)

// TestCompressToolOutputDisabled verifies that when tool output compression is
// disabled (the default), content passes through unchanged.
func TestCompressToolOutputDisabled(t *testing.T) {
	// Ensure disabled
	globalCompression.mu.Lock()
	old := globalCompression.toolOutputEnabled
	globalCompression.toolOutputEnabled = false
	globalCompression.mu.Unlock()
	defer func() {
		globalCompression.mu.Lock()
		globalCompression.toolOutputEnabled = old
		globalCompression.mu.Unlock()
	}()

	content := strings.Repeat("diff --git a/foo b/foo\n", 100)
	out, applied := compressToolOutput(content)
	if applied {
		t.Fatalf("expected applied=false when disabled, got true")
	}
	if out != content {
		t.Fatalf("expected passthrough when disabled, content changed")
	}
}

// TestCompressGitDiff verifies that git diff output is compressed by collapsing
// repeated context lines while preserving headers and hunks.
func TestCompressGitDiff(t *testing.T) {
	globalCompression.mu.Lock()
	old := globalCompression.toolOutputEnabled
	globalCompression.toolOutputEnabled = true
	globalCompression.mu.Unlock()
	defer func() {
		globalCompression.mu.Lock()
		globalCompression.toolOutputEnabled = old
		globalCompression.mu.Unlock()
	}()

	// Build a git diff with many context lines
	var sb strings.Builder
	sb.WriteString("diff --git a/foo.go b/foo.go\n")
	sb.WriteString("index abc..def 100644\n")
	sb.WriteString("--- a/foo.go\n")
	sb.WriteString("+++ b/foo.go\n")
	sb.WriteString("@@ -1,5 +1,5 @@\n")
	for i := 0; i < 20; i++ {
		sb.WriteString(" context line " + strings.Repeat("x", 50) + "\n")
	}
	sb.WriteString("+added line\n")
	sb.WriteString("-removed line\n")
	for i := 0; i < 20; i++ {
		sb.WriteString(" context line " + strings.Repeat("y", 50) + "\n")
	}

	content := sb.String()
	out, applied := compressToolOutput(content)
	if !applied {
		t.Fatalf("expected applied=true for git diff, got false")
	}
	if len(out) >= len(content) {
		t.Fatalf("expected compressed output smaller: in=%d out=%d", len(content), len(out))
	}
	// Should preserve diff header and hunk marker
	if !strings.Contains(out, "diff --git") {
		t.Errorf("compressed output missing diff header")
	}
	if !strings.Contains(out, "@@ -1,5 +1,5 @@") {
		t.Errorf("compressed output missing hunk marker")
	}
	if !strings.Contains(out, "+added line") {
		t.Errorf("compressed output missing addition")
	}
	if !strings.Contains(out, "context lines collapsed") {
		t.Errorf("expected context collapse marker in output")
	}
}

// TestCompressGrep verifies that duplicate grep matches are collapsed.
func TestCompressGrep(t *testing.T) {
	globalCompression.mu.Lock()
	old := globalCompression.toolOutputEnabled
	globalCompression.toolOutputEnabled = true
	globalCompression.mu.Unlock()
	defer func() {
		globalCompression.mu.Lock()
		globalCompression.toolOutputEnabled = old
		globalCompression.mu.Unlock()
	}()

	var sb strings.Builder
	// Pad to exceed 500-byte threshold
	sb.WriteString(strings.Repeat("# header padding line "+strings.Repeat("x", 30)+"\n", 10))
	for i := 0; i < 10; i++ {
		sb.WriteString("file.go:10:identical match with some longer content here\n")
	}
	sb.WriteString("file.go:20:unique match with different content here\n")
	for i := 0; i < 5; i++ {
		sb.WriteString("other.go:5:another identical match content\n")
	}
	content := sb.String()
	out, applied := compressToolOutput(content)
	if !applied {
		t.Fatalf("expected applied=true for grep, got false (content len=%d)", len(content))
	}
	if !strings.Contains(out, "duplicate matches collapsed") {
		t.Errorf("expected duplicate collapse marker")
	}
}

// TestCompressSmallContentSkipped verifies that small content (<500 bytes) is
// not compressed.
func TestCompressSmallContentSkipped(t *testing.T) {
	globalCompression.mu.Lock()
	old := globalCompression.toolOutputEnabled
	globalCompression.toolOutputEnabled = true
	globalCompression.mu.Unlock()
	defer func() {
		globalCompression.mu.Lock()
		globalCompression.toolOutputEnabled = old
		globalCompression.mu.Unlock()
	}()

	content := "diff --git a/foo b/foo\nsmall content"
	out, applied := compressToolOutput(content)
	if applied {
		t.Fatalf("expected small content to be skipped, got applied=true")
	}
	if out != content {
		t.Fatalf("small content was modified")
	}
}

// TestCollapseIdenticalRuns verifies that runs of 3+ identical lines collapse.
func TestCollapseIdenticalRuns(t *testing.T) {
	lines := []string{
		"header",
		"repeat",
		"repeat",
		"repeat",
		"repeat",
		"footer",
	}
	out, applied := collapseIdenticalRuns(lines)
	if !applied {
		t.Fatalf("expected applied=true")
	}
	if !strings.Contains(out, "identical lines collapsed") {
		t.Errorf("expected collapse marker, got: %s", out)
	}
	if !strings.Contains(out, "header") || !strings.Contains(out, "footer") {
		t.Errorf("expected header and footer preserved")
	}
}

// TestCavemanSuffix verifies Caveman suffix injection.
func TestCavemanSuffix(t *testing.T) {
	globalCompression.mu.Lock()
	oldEn := globalCompression.cavemanEnabled
	oldLvl := globalCompression.cavemanLevel
	globalCompression.cavemanEnabled = true
	globalCompression.cavemanLevel = "full"
	globalCompression.mu.Unlock()
	defer func() {
		globalCompression.mu.Lock()
		globalCompression.cavemanEnabled = oldEn
		globalCompression.cavemanLevel = oldLvl
		globalCompression.mu.Unlock()
	}()

	result := ApplySystemPromptSuffix("You are a helpful assistant.")
	if !strings.Contains(result, "ULTRA-TERSE") {
		t.Errorf("expected ULTRA-TERSE marker in suffix")
	}
	if !strings.HasPrefix(result, "You are a helpful assistant.") {
		t.Errorf("expected original prompt preserved at start")
	}
}

// TestCavemanDisabled verifies no suffix when disabled.
func TestCavemanDisabled(t *testing.T) {
	globalCompression.mu.Lock()
	old := globalCompression.cavemanEnabled
	globalCompression.cavemanEnabled = false
	globalCompression.mu.Unlock()
	defer func() {
		globalCompression.mu.Lock()
		globalCompression.cavemanEnabled = old
		globalCompression.mu.Unlock()
	}()

	original := "You are a helpful assistant."
	result := ApplySystemPromptSuffix(original)
	if result != original {
		t.Errorf("expected no suffix when disabled, got: %s", result)
	}
}

// TestCavemanKeepsActionCarveOut guards the fix for turns that ended with a
// short sentence describing an action instead of calling the tool. Both terse
// levels must scope brevity to prose only, otherwise the style suffix competes
// with toolExecutionGuidance and the model answers "reading the file now."
func TestCavemanKeepsActionCarveOut(t *testing.T) {
	for _, level := range []string{"light", "full"} {
		if !strings.Contains(cavemanSuffix(compressionSnapshot{
			cavemanEnabled: true,
			cavemanLevel:   level,
		}), "never to actions") {
			t.Errorf("level %q lost the action carve-out", level)
		}
	}
}
