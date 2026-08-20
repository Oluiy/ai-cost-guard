package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pterm/pterm"
)

// promptValidatedInt asks question (pre-filled with defaultValue) and
// re-prompts until the answer parses as a whole number in [min, max].
// fmt.Sscanf-style parsing was tried first and silently left zero-value
// results on malformed input (e.g. "abc" or "$5" for a budget) — this
// validates explicitly and never returns a value it didn't itself parse.
func promptValidatedInt(question string, defaultValue, min, max int) (int, error) {
	for {
		raw, err := pterm.DefaultInteractiveTextInput.
			WithDefaultValue(strconv.Itoa(defaultValue)).
			Show(question)
		if err != nil {
			return 0, err
		}
		v, perr := parseIntInRange(raw, min, max)
		if perr != nil {
			pterm.Error.Println(perr.Error())
			continue
		}
		return v, nil
	}
}

func parseIntInRange(raw string, min, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a whole number", raw)
	}
	if v < min || v > max {
		return 0, fmt.Errorf("%d is out of range (must be between %d and %d)", v, min, max)
	}
	return v, nil
}

// promptNonNegativeUSD asks question and re-prompts until the answer
// parses as a non-negative dollar amount. 0 is valid and, by convention
// elsewhere in ai-guard, means "unlimited" — callers should say so in the
// question text so that's a deliberate choice, not a parse failure in
// disguise.
func promptNonNegativeUSD(question string, defaultValue float64) (float64, error) {
	for {
		raw, err := pterm.DefaultInteractiveTextInput.
			WithDefaultValue(strconv.FormatFloat(defaultValue, 'f', -1, 64)).
			Show(question)
		if err != nil {
			return 0, err
		}
		v, perr := parseNonNegativeUSD(raw)
		if perr != nil {
			pterm.Error.Println(perr.Error())
			continue
		}
		return v, nil
	}
}

func parseNonNegativeUSD(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "$")
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a valid dollar amount — try 5 or 12.50", raw)
	}
	if v < 0 {
		return 0, fmt.Errorf("budget can't be negative, got %v", v)
	}
	return v, nil
}
