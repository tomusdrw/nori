package envcompat

import "testing"

func TestLookupFallsBackToDeploybotName(t *testing.T) {
	values := map[string]string{"DEPLOYBOT_KEY": "legacy-secret"}
	value, legacy := Lookup(func(name string) string { return values[name] }, "NORI_KEY")

	if value != "legacy-secret" {
		t.Errorf("value = %q, want legacy-secret", value)
	}
	if !legacy {
		t.Error("legacy = false, want true")
	}
}

func TestLookupPrefersNoriName(t *testing.T) {
	values := map[string]string{
		"NORI_KEY":      "canonical-secret",
		"DEPLOYBOT_KEY": "legacy-secret",
	}
	value, legacy := Lookup(func(name string) string { return values[name] }, "NORI_KEY")

	if value != "canonical-secret" {
		t.Errorf("value = %q, want canonical-secret", value)
	}
	if legacy {
		t.Error("legacy = true, want false")
	}
}
