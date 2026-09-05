package main

import (
	"regexp"
	"testing"
)

func TestDataMuxNameIsStableAndScoped(t *testing.T) {
	t.Parallel()
	first := dataMuxName("/tmp/wmux-a")
	if first != dataMuxName("/tmp/wmux-a/../wmux-a") {
		t.Fatal("equivalent data paths produced different mux names")
	}
	if first == dataMuxName("/tmp/wmux-b") {
		t.Fatal("different data paths produced the same mux name")
	}
	if !regexp.MustCompile(`^wmux-[0-9a-f]{8}$`).MatchString(first) {
		t.Fatalf("unexpected mux name %q", first)
	}
}
