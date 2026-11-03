package claude

import (
	"slices"
	"testing"

	"jendapp/internal/module"
)

func TestBuildArgsChrome(t *testing.T) {
	m := &module.Module{ID: "x"}
	if slices.Contains(buildArgs(m, "s", false), "--chrome") {
		t.Fatal("--chrome without chrome = true")
	}
	m.Claude.Chrome = true
	if !slices.Contains(buildArgs(m, "s", false), "--chrome") {
		t.Fatal("chrome = true should pass --chrome")
	}
}
