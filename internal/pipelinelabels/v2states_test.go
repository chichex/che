package pipelinelabels

import "testing"

// TestV2States_LiteralValues fija los strings literales de los 9 mapeos
// del PRD §6.c. Si algún rename del modelo v2 ocurre, este test falla y
// fuerza a auditar callers (los flows migrados en PR6b dependen de estos
// strings exactos para que `labels.Apply` encuentre las transiciones
// registradas con las mismas keys).
func TestV2States_LiteralValues(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{StateIdea, "che:state:idea"},
		{StateApplyingExplore, "che:state:applying:explore"},
		{StateExplore, "che:state:explore"},
		{StateApplyingExecute, "che:state:applying:execute"},
		{StateExecute, "che:state:execute"},
		{StateApplyingValidatePR, "che:state:applying:validate_pr"},
		{StateValidatePR, "che:state:validate_pr"},
		{StateApplyingClose, "che:state:applying:close"},
		{StateClose, "che:state:close"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// TestV2States_RoundTripParse garantiza que las constantes v2 se parsean
// correctamente por Parse (que clasifica entre KindState y KindApplying).
// Ataja un futuro caller que rompa el prefijo `che:state:applying:` por
// error y haga colisión con `che:state:`.
func TestV2States_RoundTripParse(t *testing.T) {
	cases := []struct {
		label    string
		wantKind Kind
		wantStep string
	}{
		{StateIdea, KindState, "idea"},
		{StateApplyingExplore, KindApplying, "explore"},
		{StateExplore, KindState, "explore"},
		{StateApplyingExecute, KindApplying, "execute"},
		{StateExecute, KindState, "execute"},
		{StateApplyingValidatePR, KindApplying, "validate_pr"},
		{StateValidatePR, KindState, "validate_pr"},
		{StateApplyingClose, KindApplying, "close"},
		{StateClose, KindState, "close"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			p, err := Parse(c.label)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.label, err)
			}
			if p.Kind != c.wantKind {
				t.Errorf("Kind: got %v, want %v", p.Kind, c.wantKind)
			}
			if p.Step != c.wantStep {
				t.Errorf("Step: got %q, want %q", p.Step, c.wantStep)
			}
		})
	}
}
