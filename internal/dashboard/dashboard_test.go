package dashboard

import "testing"

// csvSafe defuses CSV/formula injection (CWE-1236) in the report export —
// `model` in particular is fully attacker-controlled (any
// /v1/chat/completions caller sets it), so this is a real, not
// hypothetical, injection vector into whatever spreadsheet tool an admin
// opens the exported report with.
func TestCSVSafe_PrefixesFormulaTriggerCharacters(t *testing.T) {
	for _, in := range []string{
		"=cmd|' /C calc'!A0",
		"+1+1",
		"-1+1",
		"@SUM(1+1)",
	} {
		got := csvSafe(in)
		if got == in {
			t.Errorf("csvSafe(%q) left the value unescaped, want a leading quote", in)
		}
		if got[0] != '\'' {
			t.Errorf("csvSafe(%q) = %q, want it prefixed with a single quote", in, got)
		}
		if got[1:] != in {
			t.Errorf("csvSafe(%q) = %q, want original value preserved after the prefix", in, got)
		}
	}
}

func TestCSVSafe_LeavesOrdinaryValuesUnchanged(t *testing.T) {
	for _, in := range []string{"gpt-4o", "user_123", "claude-3-haiku", ""} {
		if got := csvSafe(in); got != in {
			t.Errorf("csvSafe(%q) = %q, want unchanged", in, got)
		}
	}
}
