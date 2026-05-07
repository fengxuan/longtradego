package core

import (
	"reflect"
	"testing"
)

func TestInferCommandMetadataSkillRunSymbolsFlag(t *testing.T) {
	command, symbols := InferCommandMetadata([]string{
		"skill", "run",
		"--text", "screen tech stocks",
		"--symbols", "700.HK,9988.HK,IBM.US",
		"--dry-run",
	})
	if command != "skill" {
		t.Fatalf("expected command skill, got %q", command)
	}
	expected := []string{"700.HK", "9988.HK", "IBM.US"}
	if !reflect.DeepEqual(symbols, expected) {
		t.Fatalf("unexpected symbols: got=%v want=%v", symbols, expected)
	}
}

func TestInferCommandMetadataSkillRunSymbolsInlineFlag(t *testing.T) {
	command, symbols := InferCommandMetadata([]string{
		"skill", "run",
		"--text", "screen tech stocks",
		"--symbols=1810.HK,9999.HK",
	})
	if command != "skill" {
		t.Fatalf("expected command skill, got %q", command)
	}
	expected := []string{"1810.HK", "9999.HK"}
	if !reflect.DeepEqual(symbols, expected) {
		t.Fatalf("unexpected symbols: got=%v want=%v", symbols, expected)
	}
}
