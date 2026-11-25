package version

import "testing"

func TestStringFallsBackToDev(t *testing.T) {
	old := Value
	defer func() { Value = old }()
	Value = ""
	if got := String(); got != "dev" {
		t.Fatalf("String() = %q, want dev fallback", got)
	}
	Value = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Fatalf("String() = %q, want injected value", got)
	}
}
