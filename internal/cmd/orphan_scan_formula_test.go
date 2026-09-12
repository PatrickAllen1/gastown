package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

func TestMolOrphanScanScopeDefaultAndExplicitOverride(t *testing.T) {
	content, err := formula.GetEmbeddedFormulaContent("mol-orphan-scan")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-orphan-scan): %v", err)
	}

	f, err := formula.Parse(content)
	if err != nil {
		t.Fatalf("Parse(mol-orphan-scan): %v", err)
	}

	for _, tc := range []struct {
		name      string
		overrides []string
		want      string
	}{
		{name: "default scope", want: "town"},
		{name: "explicit town override", overrides: []string{"scope=town"}, want: "town"},
		{name: "explicit rig override", overrides: []string{"scope=beads"}, want: "beads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFormulaVarMap(f, tc.overrides)["scope"]
			if got != tc.want {
				t.Fatalf("scope = %q, want %q", got, tc.want)
			}
		})
	}
}
