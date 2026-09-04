package version

import "testing"

func TestBuildMetadataAlwaysHasFallbacks(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must have a development fallback")
	}
	if Commit == "" {
		t.Fatal("Commit must have an unknown fallback")
	}
}
