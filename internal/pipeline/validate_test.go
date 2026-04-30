package pipeline

import (
	"errors"
	"strings"
	"testing"
)

// TestValidate_Default es el sanity check del happy path: el built-in
// `Default()` tiene que pasar Validate sin errores. Si rompe, alguien
// metió una regla nueva y se olvidó de actualizar Default().
func TestValidate_Default(t *testing.T) {
	if err := Validate(Default()); err != nil {
		t.Fatalf("Validate(Default()) = %v, want nil", err)
	}
}

// TestValidate_StepNameRules cubre la matriz de nombres permitidos:
// el regex tiene que matchear lo mismo que el parser de markers
// (`[goto: <name>]`). Si esto desincroniza, el wizard podría aceptar
// nombres que el parser no resuelve.
func TestValidate_StepNameRules(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"plain", true},
		{"with_underscore", true},
		{"_leading_underscore", true},
		{"with123numbers", true},
		{"", false},                  // vacío
		{"With-Dash", false},         // dash no permitido
		{"WithUpper", false},         // mayúsculas no
		{"123leading_digit", false},  // empieza con dígito
		{"with space", false},
		{"with.dot", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Pipeline{
				Version: CurrentVersion,
				Steps:   []Step{{Name: tc.name, Agents: []string{"claude-opus"}}},
			}
			err := Validate(p)
			if tc.ok && err != nil {
				t.Errorf("name %q: Validate = %v, want nil", tc.name, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("name %q: Validate = nil, want error", tc.name)
			}
		})
	}
}

// TestValidate_DuplicateStepName: nombres duplicados harían ambiguo
// `[goto: <name>]`. La validación tiene que rechazar y apuntar al
// índice exacto del segundo step duplicado.
func TestValidate_DuplicateStepName(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Steps: []Step{
			{Name: "a", Agents: []string{"x"}},
			{Name: "b", Agents: []string{"x"}},
			{Name: "a", Agents: []string{"x"}},
		},
	}
	err := Validate(p)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field != "steps[2].name" {
		t.Errorf("Field = %q, want steps[2].name", le.Field)
	}
	if !strings.Contains(le.Reason, "duplicate") {
		t.Errorf("Reason = %q, expected 'duplicate'", le.Reason)
	}
}

// TestValidate_EmptyAgents: un step sin agentes no puede correr nada.
// La validación tiene que apuntar a `steps[i].agents`.
func TestValidate_EmptyAgents(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Steps: []Step{
			{Name: "x", Agents: []string{}},
		},
	}
	err := Validate(p)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T, want *LoadError", err)
	}
	if le.Field != "steps[0].agents" {
		t.Errorf("Field = %q, want steps[0].agents", le.Field)
	}
}

// TestValidate_EmptyAgentRef: un agente con string vacío en el array
// es probablemente un typo (trailing comma + edición a mano). El error
// tiene que apuntar al índice del array, no al step entero.
func TestValidate_EmptyAgentRef(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Steps: []Step{
			{Name: "x", Agents: []string{"claude-opus", "", "claude-sonnet"}},
		},
	}
	err := Validate(p)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T, want *LoadError", err)
	}
	if le.Field != "steps[0].agents[1]" {
		t.Errorf("Field = %q, want steps[0].agents[1]", le.Field)
	}
}

// TestValidate_AggregatorPresets: aggregators custom (no en el preset
// de 3) son típicamente typos ("first-blocker" con dash, "majority "
// con trailing space). El error tiene que listar las opciones válidas.
func TestValidate_AggregatorPresets(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Steps: []Step{
			{Name: "x", Agents: []string{"a", "b"}, Aggregator: "first-blocker"},
		},
	}
	err := Validate(p)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T, want *LoadError", err)
	}
	if le.Field != "steps[0].aggregator" {
		t.Errorf("Field = %q, want steps[0].aggregator", le.Field)
	}
	for _, want := range []string{"majority", "unanimous", "first_blocker"} {
		if !strings.Contains(le.Reason, want) {
			t.Errorf("Reason missing preset %q: %s", want, le.Reason)
		}
	}
}

// TestValidate_AggregatorEmpty: `omitempty` permite que el campo no
// venga; el motor lo trata como AggregatorMajority. Validate no tiene
// que rechazar ese caso aunque len(agents)==1 — el wizard alterna
// entre 1 y N agentes y forzar quitar el campo cada vez sería
// friccionado.
func TestValidate_AggregatorEmpty(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Steps: []Step{
			{Name: "single", Agents: []string{"x"}, Aggregator: ""},
			{Name: "multi", Agents: []string{"x", "y"}, Aggregator: ""},
		},
	}
	if err := Validate(p); err != nil {
		t.Errorf("Validate = %v, want nil for empty aggregator", err)
	}
}

// TestValidate_EntryRequiresAgents: si el pipeline declara `entry`
// (struct presente, no nil), la slice de agents no puede estar vacía.
// Schema-side eso lo cubre el `required: ["agents"]`, pero el loader
// tiene que coincidir para el caso de un struct construido por código
// (wizard, simulate, etc).
func TestValidate_EntryRequiresAgents(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Entry:   &Entry{Agents: []string{}},
		Steps:   []Step{{Name: "x", Agents: []string{"a"}}},
	}
	err := Validate(p)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T, want *LoadError", err)
	}
	if le.Field != "entry.agents" {
		t.Errorf("Field = %q, want entry.agents", le.Field)
	}
}

// TestValidate_EntryAggregatorPresets: misma regla que para Step pero
// scope al entry — mensaje y campo distinto.
func TestValidate_EntryAggregatorPresets(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Entry:   &Entry{Agents: []string{"a"}, Aggregator: "weighted"},
		Steps:   []Step{{Name: "x", Agents: []string{"a"}}},
	}
	err := Validate(p)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T, want *LoadError", err)
	}
	if le.Field != "entry.aggregator" {
		t.Errorf("Field = %q, want entry.aggregator", le.Field)
	}
}

// TestValidateAgents_Default: el built-in `Default()` referencia
// agentes custom (`plan-reviewer-strict` etc) que un repo virgen no
// tiene. Si el caller pasa una closure que sólo reconoce
// claude-opus/sonnet/haiku, ValidateAgents debería emitir un dangling
// ref con Field apuntando al primer slot custom (steps[2]).
func TestValidateAgents_Default(t *testing.T) {
	hasOnlyBuiltin := func(name string) bool {
		switch name {
		case "claude-opus", "claude-sonnet", "claude-haiku":
			return true
		}
		return false
	}
	err := ValidateAgents(Default(), hasOnlyBuiltin)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError (custom agents in default)", err, err)
	}
	if !strings.Contains(le.Field, "steps[2]") {
		t.Errorf("Field = %q, expected to point to steps[2] (validate_issue, first slot with custom agents)", le.Field)
	}
}

// TestValidateAgents_NilPredicate: pasar `nil` apaga la verificación.
// Esto es importante para call sites que no tienen registry handy
// (preview del wizard, dry-run sin filesystem) — no quieren error
// "registry no provided", quieren skip.
func TestValidateAgents_NilPredicate(t *testing.T) {
	if err := ValidateAgents(Default(), nil); err != nil {
		t.Errorf("ValidateAgents(_, nil) = %v, want nil", err)
	}
}

// TestValidateAgents_AllResolved: caso happy — la closure dice "todo
// existe", no hay errores. Cubre el path post-Validate de un pipeline
// custom donde el usuario ya configuró sus agents.
func TestValidateAgents_AllResolved(t *testing.T) {
	always := func(string) bool { return true }
	if err := ValidateAgents(Default(), always); err != nil {
		t.Errorf("ValidateAgents = %v, want nil", err)
	}
}

// TestValidateAgents_EntryRef: si el entry referencia un agente que no
// existe, el error tiene que apuntar a entry.agents[N], no a steps[].
func TestValidateAgents_EntryRef(t *testing.T) {
	p := Pipeline{
		Version: CurrentVersion,
		Entry:   &Entry{Agents: []string{"missing-gate"}},
		Steps:   []Step{{Name: "x", Agents: []string{"claude-opus"}}},
	}
	has := func(n string) bool { return n == "claude-opus" }
	err := ValidateAgents(p, has)
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err type = %T (%v), want *LoadError", err, err)
	}
	if le.Field != "entry.agents[0]" {
		t.Errorf("Field = %q, want entry.agents[0]", le.Field)
	}
}
