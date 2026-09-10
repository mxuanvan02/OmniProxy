package proxy

import (
	"reflect"
	"testing"
)

func TestCapabilityEndpointCatalogPublishesEverySupportedRoute(t *testing.T) {
	got := capabilityEndpointCatalog()
	want := map[string][]string{
		capabilityChat: {
			"/v1/chat/completions",
			"/v1/messages",
			"/v1/responses",
		},
		capabilitySearch: {
			"/v1/search",
		},
		capabilityImage: {
			"/v1/images/generations",
			"/v1/images/edits",
			"/v1/images/variations",
		},
		capabilityEmbedding: {
			"/v1/embeddings",
		},
		capabilityAudioSTT: {
			"/v1/audio/transcriptions",
			"/v1/audio/translations",
		},
		capabilityAudioTTS: {
			"/v1/audio/speech",
		},
		capabilityModeration: {
			"/v1/moderations",
		},
		capabilityVideo: {
			"/v1/videos/generations",
			"/v1/videos/{id}",
		},
		capabilityAudioMusic: {
			"/v1/music/generations",
			"/v1/music/{id}",
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoint catalog mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestCapabilityPrimaryEndpointStaysBackwardCompatible(t *testing.T) {
	catalog := capabilityEndpointCatalog()
	for capability, endpoints := range catalog {
		if len(endpoints) == 0 {
			t.Fatalf("capability %q has no endpoints", capability)
		}
		if got := primaryCapabilityEndpoint(capability, catalog); got != endpoints[0] {
			t.Errorf("primary endpoint for %q = %q, want %q", capability, got, endpoints[0])
		}
	}
	if got := primaryCapabilityEndpoint("unknown", catalog); got != "" {
		t.Errorf("unknown capability primary endpoint = %q, want empty", got)
	}
}
