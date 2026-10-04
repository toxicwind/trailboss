package models

import "testing"

func TestRecordedBranch(t *testing.T) {
	r := Repo{
		DefaultBranch:  "main",
		BranchPresence: map[string]bool{"feature": true, "stale": false},
	}
	// Default branch is always present and known.
	if has, known := r.RecordedBranch("main"); !has || !known {
		t.Errorf("RecordedBranch(main) = (%v, %v), want (true, true)", has, known)
	}
	// Recorded present branch.
	if has, known := r.RecordedBranch("feature"); !has || !known {
		t.Errorf("RecordedBranch(feature) = (%v, %v), want (true, true)", has, known)
	}
	// Recorded absent branch.
	if has, known := r.RecordedBranch("stale"); has || !known {
		t.Errorf("RecordedBranch(stale) = (%v, %v), want (false, true)", has, known)
	}
	// Never-checked branch: unknown.
	if has, known := r.RecordedBranch("nope"); has || known {
		t.Errorf("RecordedBranch(nope) = (%v, %v), want (false, false)", has, known)
	}
	// Empty branch name falls through to the map lookup.
	if _, known := r.RecordedBranch(""); known {
		t.Errorf("RecordedBranch(\"\") known = true, want false")
	}
}

func TestSignModes(t *testing.T) {
	modes := SignModes()
	if len(modes) == 0 {
		t.Fatal("SignModes() returned no modes")
	}
	for _, m := range modes {
		if !IsValidSignMode(m) {
			t.Errorf("SignModes() returned %q which IsValidSignMode rejects", m)
		}
	}
	if IsValidSignMode("bogus-mode") {
		t.Error("IsValidSignMode(bogus-mode) = true, want false")
	}
}

func TestTokenEnvVar(t *testing.T) {
	if TokenEnvVar == "" {
		t.Error("TokenEnvVar is empty")
	}
}
