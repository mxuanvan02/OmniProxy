package proxy

import (
	"omniproxy/config"
	"testing"
)

// The Codex image_generation tool only accepts the gpt-image-* family as its
// "model" field. A text model stored on the account (legacy value) must never
// be forwarded, otherwise the upstream rejects the request.
func TestCodexImageToolModel_RejectsTextModels(t *testing.T) {
	cases := []struct {
		name      string
		configured string
		requested string
		want      string
	}{
		{"empty falls back to default", "", "", defaultCodexImageToolModel},
		{"configured image model wins", "gpt-image-1", "", "gpt-image-1"},
		{"legacy text model on account is ignored", "gpt-5.6-luna", "", defaultCodexImageToolModel},
		{"legacy text model ignored, requested image model used", "gpt-5.4", "gpt-image-1-mini", "gpt-image-1-mini"},
		{"requested text model is ignored", "", "gpt-5.6-sol", defaultCodexImageToolModel},
		{"configured beats requested", "gpt-image-1", "gpt-image-2", "gpt-image-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct := &config.Account{ID: "a", AuthMethod: "codex", CodexImageModel: tc.configured}
			if got := codexImageToolModel(acct, tc.requested); got != tc.want {
				t.Fatalf("codexImageToolModel(%q, %q) = %q, want %q", tc.configured, tc.requested, got, tc.want)
			}
		})
	}
}

func TestCodexImageToolModel_NilAccount(t *testing.T) {
	if got := codexImageToolModel(nil, ""); got != defaultCodexImageToolModel {
		t.Fatalf("nil account = %q, want %q", got, defaultCodexImageToolModel)
	}
	if got := codexImageToolModel(nil, "gpt-image-1"); got != "gpt-image-1" {
		t.Fatalf("nil account with requested image model = %q, want gpt-image-1", got)
	}
}

// A Codex account's default image model must be an image model, not the
// generic text default used by other providers.
func TestImageModelFor_CodexDefaultsToImageModel(t *testing.T) {
	codex := &config.Account{ID: "c", AuthMethod: "codex"}
	if got := imageModelFor(codex, ""); got != defaultCodexImageToolModel {
		t.Fatalf("codex default = %q, want %q", got, defaultCodexImageToolModel)
	}
	// Explicit request still wins.
	if got := imageModelFor(codex, "gpt-image-1"); got != "gpt-image-1" {
		t.Fatalf("explicit request = %q, want gpt-image-1", got)
	}
	// Non-codex accounts keep the generic default.
	other := &config.Account{ID: "o", AuthMethod: "external_openai"}
	if got := imageModelFor(other, ""); got != defaultImageModel {
		t.Fatalf("non-codex default = %q, want %q", got, defaultImageModel)
	}
}

func TestIsCodexImageToolModel(t *testing.T) {
	valid := []string{"gpt-image-2", "gpt-image-1", "gpt-image-1-mini", "  gpt-image-2  "}
	for _, v := range valid {
		if !isCodexImageToolModel(v) {
			t.Fatalf("%q should be a valid codex image model", v)
		}
	}
	invalid := []string{"", "gpt-5.6-luna", "gpt-5.5", "dall-e-3", "image-gpt"}
	for _, v := range invalid {
		if isCodexImageToolModel(v) {
			t.Fatalf("%q should NOT be a valid codex image model", v)
		}
	}
}

// The catalog offered to the operator must contain only image models, with
// gpt-image-2 first (current default).
func TestCodexImageToolModels_CatalogShape(t *testing.T) {
	models := codexImageToolModels()
	if len(models) == 0 {
		t.Fatal("catalog is empty")
	}
	if models[0].ID != defaultCodexImageToolModel {
		t.Fatalf("first model = %q, want default %q", models[0].ID, defaultCodexImageToolModel)
	}
	seen := map[string]bool{}
	for _, m := range models {
		if !isCodexImageToolModel(m.ID) {
			t.Fatalf("catalog contains non-image model %q", m.ID)
		}
		if m.Name == "" {
			t.Fatalf("model %q has empty name", m.ID)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate model %q", m.ID)
		}
		seen[m.ID] = true
	}
}
