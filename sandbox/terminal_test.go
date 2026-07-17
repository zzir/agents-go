package sandbox

import "testing"

func TestTerminalOptionsDefaults(t *testing.T) {
	var o TerminalOptions
	if got := o.EffectiveCols(); got != DefaultTerminalCols {
		t.Errorf("EffectiveCols() = %d, want %d", got, DefaultTerminalCols)
	}
	if got := o.EffectiveRows(); got != DefaultTerminalRows {
		t.Errorf("EffectiveRows() = %d, want %d", got, DefaultTerminalRows)
	}
	if got := o.EffectiveTerm(); got != DefaultTerminalTerm {
		t.Errorf("EffectiveTerm() = %q, want %q", got, DefaultTerminalTerm)
	}
}

func TestTerminalOptionsExplicit(t *testing.T) {
	o := TerminalOptions{Cols: 120, Rows: 40, Term: "vt100"}
	if got := o.EffectiveCols(); got != 120 {
		t.Errorf("EffectiveCols() = %d, want 120", got)
	}
	if got := o.EffectiveRows(); got != 40 {
		t.Errorf("EffectiveRows() = %d, want 40", got)
	}
	if got := o.EffectiveTerm(); got != "vt100" {
		t.Errorf("EffectiveTerm() = %q, want vt100", got)
	}
}

func TestTerminalOptionsNegativeSize(t *testing.T) {
	o := TerminalOptions{Cols: -1, Rows: -1}
	if got := o.EffectiveCols(); got != DefaultTerminalCols {
		t.Errorf("EffectiveCols() = %d, want %d", got, DefaultTerminalCols)
	}
	if got := o.EffectiveRows(); got != DefaultTerminalRows {
		t.Errorf("EffectiveRows() = %d, want %d", got, DefaultTerminalRows)
	}
}
