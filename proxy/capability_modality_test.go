package proxy

import (
	"strings"
	"testing"
)

// Model IDs taken verbatim from the live aggregated catalog
// (/v1/models?catalog=all) on 2026-09-10. Underscore-separated IDs come from
// the Gommo AutoAI account and were the ones that previously fell through to
// the chat default, which is what put video generators and TTS voices into
// chat model pickers.
func TestClassifyModelCapabilitiesRealCatalog(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		// --- video: underscore families with no hyphenated marker ---
		{"veo_3_1", capabilityVideo},
		{"veo_omni", capabilityVideo},
		{"veo_omni_edit", capabilityVideo},
		{"kling_video_3_0", capabilityVideo},
		{"kling_video_2_1_10s", capabilityVideo},
		{"hailuo_2_3", capabilityVideo},
		{"grok_video_heavy", capabilityVideo},
		{"topaz_video_upscale", capabilityVideo},
		{"video_upscale_1_0", capabilityVideo},
		// hyphenated spellings must keep working
		{"veo-3.1", capabilityVideo},
		{"sora-2", capabilityVideo},

		// --- image generation ---
		{"google_image_gen_banana", capabilityImage},
		{"google_image_gen_banana_pro_reason", capabilityImage},
		{"google_image_gen_3_5", capabilityImage},
		{"flux_3", capabilityImage},
		{"midjourney_8_2", capabilityImage},
		{"seedream_5_0", capabilityImage},
		{"grok_imagine", capabilityImage},
		{"topaz_image_upscale", capabilityImage},
		{"z_image", capabilityImage},
		{"gpt-image-1.5", capabilityImage},

		// --- speech synthesis ---
		{"minimax_speech_2_8_hd", capabilityAudioTTS},
		{"minimax_speech_2_6_turbo", capabilityAudioTTS},
		{"autoai_speech_1", capabilityAudioTTS},
		{"mimo-v2.5-tts-voicedesign", capabilityAudioTTS},

		// --- music generation ---
		{"suno-v4.5", capabilityAudioMusic},
		{"suno-v5.5", capabilityAudioMusic},

		// --- embeddings ---
		{"Qwen3-Embedding-8B", capabilityEmbedding},
		{"gemini-embedding-001", capabilityEmbedding},

		// --- chat: must NOT be reclassified as media ---
		{"gpt-6-astra", capabilityChat},
		{"gpt-5.6-sol", capabilityChat},
		{"o3", capabilityChat},
		{"codex-mini-latest", capabilityChat},
		{"claude-opus-4-6", capabilityChat},
		{"claude-opus-4-6-thinking", capabilityChat},
		{"anthropic/claude-opus-4-6", capabilityChat},
		{"glm-5.3", capabilityChat},
		{"glm-5.2-anthropic", capabilityChat},
		{"deepseek-v4-pro", capabilityChat},
		{"[opencode]deepseek-v4-flash", capabilityChat},
		{"kimi-k3", capabilityChat},
		{"minimax-m3", capabilityChat},
		{"MiniMax-M2.7", capabilityChat},
		{"qwen3.8-max", capabilityChat},
		{"doubao-seed-2-0-pro", capabilityChat},
		{"gemini-3.1-pro-preview", capabilityChat},
		{"longcat-2.0", capabilityChat},
		{"step-3.7-flash", capabilityChat},
		{"3.0-omni", capabilityChat},
	}

	for _, tc := range cases {
		got := classifyModelCapabilities(tc.id)
		if len(got) == 0 {
			t.Errorf("%s: no capability returned", tc.id)
			continue
		}
		// The primary (non-chat) signal decides the request shape.
		primary := got[0]
		for _, capability := range got {
			if capability != capabilityChat {
				primary = capability
				break
			}
		}
		if primary != tc.want {
			t.Errorf("%s: primary capability = %q, want %q (full set %v)",
				tc.id, primary, tc.want, got)
		}
	}
}

// A speech-to-text model must not be captured by the bare "speech" needle the
// TTS rule now uses.
func TestSpeechToTextNotClassifiedAsTTS(t *testing.T) {
	for _, id := range []string{
		"whisper-1",
		"gpt-4o-transcribe",
		"speech-to-text-v2",
		"minimax_asr_1",
	} {
		got := classifyModelCapabilities(id)
		for _, capability := range got {
			if capability == capabilityAudioTTS {
				t.Errorf("%s classified as TTS (set %v); must stay STT", id, got)
			}
		}
	}
}

// Vision input must never be read as image generation. gpt-6-astra accepts
// images and emits text; publishing it as an image model sent it to the wrong
// endpoint.
func TestVisionInputIsNotImageGeneration(t *testing.T) {
	info := ModelInfo{
		ModelId:    "gpt-6-astra",
		Provider:   "openai-codex",
		InputTypes: []string{"text", "image"},
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, true, &info)

	if got := entry["capability"]; got != capabilityChat {
		t.Fatalf("capability = %v, want chat", got)
	}
	capabilities, ok := entry["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("capabilities block missing")
	}
	if capabilities["image"] != false {
		t.Errorf("capabilities.image = %v; vision input must not claim image output", capabilities["image"])
	}
	if capabilities["vision"] != true {
		t.Errorf("capabilities.vision = %v, want true", capabilities["vision"])
	}
	modalities, ok := entry["modalities"].(map[string][]string)
	if !ok {
		t.Fatal("modalities block missing")
	}
	if len(modalities["output"]) != 1 || modalities["output"][0] != "text" {
		t.Errorf("output modality = %v, want [text]", modalities["output"])
	}
}

// Non-chat models must not advertise chat-only affordances.
func TestMediaModelsDoNotAdvertiseChatAffordances(t *testing.T) {
	cases := []struct {
		id         string
		wantOutput string
	}{
		{"kling_video_3_0", "video"},
		{"google_image_gen_banana_pro", "image"},
		{"minimax_speech_2_8_hd", "audio"},
		{"suno-v5.5", "audio"},
		{"gemini-embedding-001", "embedding"},
	}

	for _, tc := range cases {
		info := ModelInfo{ModelId: tc.id, Provider: "external", External: true}
		entry := buildModelInfoWithTokenLimits(tc.id, info.Provider, false, &info)

		capabilities, ok := entry["capabilities"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: capabilities block missing", tc.id)
		}
		if tool, _ := capabilities["tool_use"].(map[string]bool); tool["supported"] {
			t.Errorf("%s advertises tool_use", tc.id)
		}
		if stream, _ := capabilities["streaming"].(map[string]bool); stream["supported"] {
			t.Errorf("%s advertises streaming", tc.id)
		}
		if reasoning, _ := capabilities["reasoning"].(map[string]bool); reasoning["supported"] {
			t.Errorf("%s advertises reasoning", tc.id)
		}
		if _, present := entry["runtime"]; present {
			t.Errorf("%s publishes reasoning-effort runtime block", tc.id)
		}
		modalities, ok := entry["modalities"].(map[string][]string)
		if !ok {
			t.Fatalf("%s: modalities block missing", tc.id)
		}
		if len(modalities["output"]) != 1 || modalities["output"][0] != tc.wantOutput {
			t.Errorf("%s output modality = %v, want [%s]",
				tc.id, modalities["output"], tc.wantOutput)
		}
	}
}

// Chat models keep every affordance they had before modality labelling.
func TestChatModelsKeepChatAffordances(t *testing.T) {
	info := ModelInfo{ModelId: "glm-5.3", Provider: "custom", External: true}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)

	capabilities, ok := entry["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("capabilities block missing")
	}
	if tool, _ := capabilities["tool_use"].(map[string]bool); !tool["supported"] {
		t.Error("chat model lost tool_use")
	}
	if stream, _ := capabilities["streaming"].(map[string]bool); !stream["supported"] {
		t.Error("chat model lost streaming")
	}
	reasoning, ok := capabilities["reasoning"].(map[string]interface{})
	if !ok || reasoning["supported"] != true {
		t.Errorf("chat model lost reasoning: %#v", capabilities["reasoning"])
	}
	if _, present := entry["runtime"]; !present {
		t.Error("chat model lost reasoning-effort runtime block")
	}
}

// Provider-declared output modality outranks name inference: an ID with no
// recognisable token must still be classified from its metadata.
func TestExplicitOutputModalityWinsOverName(t *testing.T) {
	info := ModelInfo{
		ModelId:     "mystery-model-7",
		Provider:    "external",
		External:    true,
		OutputTypes: []string{"video"},
	}
	entry := buildModelInfoWithTokenLimits(info.ModelId, info.Provider, false, &info)
	if got := entry["capability"]; got != capabilityVideo {
		t.Fatalf("capability = %v, want video from declared output modality", got)
	}
}

func TestFilterModelsByCapability(t *testing.T) {
	models := []map[string]interface{}{
		{"id": "glm-5.3", "capability": capabilityChat},
		{"id": "kling_video_3_0", "capability": capabilityVideo},
		{"id": "flux_3", "capability": capabilityImage},
		{"id": "suno-v5.5", "capability": capabilityAudioMusic},
		// No capability field: legacy entry, must be treated as chat.
		{"id": "legacy-entry"},
	}

	// Empty selector is a no-op — default route keeps its historical shape.
	if got := filterModelsByCapability(models, ""); len(got) != len(models) {
		t.Errorf("empty selector returned %d entries, want %d", len(got), len(models))
	}

	chat := filterModelsByCapability(models, "chat")
	if len(chat) != 2 {
		t.Errorf("capability=chat returned %d entries, want 2 (glm-5.3 + legacy)", len(chat))
	}

	media := filterModelsByCapability(models, "video,image")
	if len(media) != 2 {
		t.Errorf("capability=video,image returned %d entries, want 2", len(media))
	}
	for _, model := range media {
		id, _ := model["id"].(string)
		if id == "glm-5.3" || id == "suno-v5.5" {
			t.Errorf("media filter leaked %s", id)
		}
	}

	// A typo must not hand a chat picker the whole catalogue.
	if got := filterModelsByCapability(models, "chatt"); len(got) != 0 {
		t.Errorf("unknown capability returned %d entries, want 0", len(got))
	}

	// Case and spacing tolerance.
	if got := filterModelsByCapability(models, " CHAT , Video "); len(got) != 3 {
		t.Errorf("mixed-case selector returned %d entries, want 3", len(got))
	}
}

func TestNormalizeModelIDForClassification(t *testing.T) {
	cases := map[string]string{
		"google_image_gen_banana":   "google-image-gen-banana",
		"veo_3_1":                   "veo-3-1",
		"MiniMax-M2.7":              "minimax-m2-7",
		"anthropic/claude-opus-4-6": "anthropic-claude-opus-4-6",
		"  gpt-6-astra  ":           "gpt-6-astra",
		"gpt__image":                "gpt-image",
	}
	for in, want := range cases {
		if got := normalizeModelIDForClassification(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := normalizeModelIDForClassification("   "); got != "" {
		t.Errorf("blank input returned %q", got)
	}
	if !strings.EqualFold(normalizeModelIDForClassification("VEO_3_1"), "veo-3-1") {
		t.Error("normalize must lowercase")
	}
}
