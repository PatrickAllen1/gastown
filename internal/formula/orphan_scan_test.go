package formula

import (
	"strings"
	"testing"
)

func TestMolOrphanScanScopeIsOptionalWithTownDefault(t *testing.T) {
	content, err := GetEmbeddedFormulaContent("mol-orphan-scan")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-orphan-scan): %v", err)
	}

	f, err := Parse(content)
	if err != nil {
		t.Fatalf("Parse(mol-orphan-scan): %v", err)
	}

	scope, ok := f.Vars["scope"]
	if !ok {
		t.Fatal("mol-orphan-scan is missing the scope variable")
	}
	if scope.Required {
		t.Fatal("scope must be optional when it has a default")
	}
	if scope.Default != "town" {
		t.Fatalf("scope default = %q, want %q", scope.Default, "town")
	}
}

func TestMolOrphanScanCrewLaneSafetyContract(t *testing.T) {
	content, err := GetEmbeddedFormulaContent("mol-orphan-scan")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-orphan-scan): %v", err)
	}

	f, err := Parse(content)
	if err != nil {
		t.Fatalf("Parse(mol-orphan-scan): %v", err)
	}

	var formulaText strings.Builder
	formulaText.WriteString(f.Description)
	for _, step := range f.Steps {
		formulaText.WriteString("\n")
		formulaText.WriteString(step.Description)
	}
	text := strings.ToLower(formulaText.String())

	for _, want := range []string{
		"nested crew",
		"writer/reviewer handle",
		"authoritative bead",
		"gt session list",
		"gt session status",
		"live_nested_handle",
		"missing_handle",
		"uncertain",
		"worker custody",
		"do not reset",
		"do not reassign",
		"do not close",
		"do not burn",
		"do not kill",
		"do not restart",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("mol-orphan-scan safety contract is missing %q", want)
		}
	}
}
