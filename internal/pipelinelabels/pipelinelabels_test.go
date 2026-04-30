package pipelinelabels

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chichex/che/internal/pipeline"
)

// TestExpected_DefaultGolden es el golden bit-perfect de Expected() sobre
// pipeline.Default(). Si Default() cambia (se agrega/saca un step en
// internal/pipeline), este test falla — es la señal de que hay que
// regenerar la lista esperada arriba.
//
// Mantenemos el golden inline (no en testdata/) porque es chico y porque
// queremos que el diff del PR muestre el cambio sin abrir un archivo
// aparte. Si el set crece a >20 entries lo movemos a testdata.
func TestExpected_DefaultGolden(t *testing.T) {
	got := Expected(pipeline.Default())

	want := []string{
		"che:state:idea",
		"che:state:applying:idea",
		"che:state:explore",
		"che:state:applying:explore",
		"che:state:validate_issue",
		"che:state:applying:validate_issue",
		"che:state:execute",
		"che:state:applying:execute",
		"che:state:validate_pr",
		"che:state:applying:validate_pr",
		"che:state:close",
		"che:state:applying:close",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expected(Default()) drift\n--- got ---\n%v\n--- want ---\n%v", got, want)
	}
}

// TestExpected_OrderAndPairing chequea las dos invariantes estructurales
// de Expected: orden por step (mismo orden que pipeline.Steps) y pairing
// (state precede a applying dentro de cada step). Es defensa contra un
// refactor que reordene el slice y rompa callers que asumen el orden.
func TestExpected_OrderAndPairing(t *testing.T) {
	p := pipeline.Pipeline{
		Version: pipeline.CurrentVersion,
		Steps: []pipeline.Step{
			{Name: "alpha", Agents: []string{"claude-opus"}},
			{Name: "beta", Agents: []string{"claude-opus"}},
			{Name: "gamma", Agents: []string{"claude-opus"}},
		},
	}
	got := Expected(p)
	want := []string{
		"che:state:alpha",
		"che:state:applying:alpha",
		"che:state:beta",
		"che:state:applying:beta",
		"che:state:gamma",
		"che:state:applying:gamma",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Expected drift\n got: %v\nwant: %v", got, want)
	}
}

// TestExpected_EmptyPipeline confirma que Expected sobre un pipeline sin
// steps devuelve el slice vacío (no nil panics). El loader de pipeline ya
// rechaza pipelines vacíos en producción, pero el contrato de Expected
// debe ser robusto.
func TestExpected_EmptyPipeline(t *testing.T) {
	p := pipeline.Pipeline{Version: pipeline.CurrentVersion}
	got := Expected(p)
	if len(got) != 0 {
		t.Errorf("Expected(empty) = %v, want empty slice", got)
	}
}

// TestParse_StateRoundTrip: generar `che:state:<step>` con StateLabel y
// parsearlo de vuelta debe devolver el mismo step name.
func TestParse_StateRoundTrip(t *testing.T) {
	cases := []string{"idea", "explore", "validate_issue", "execute", "validate_pr", "close"}
	for _, step := range cases {
		t.Run(step, func(t *testing.T) {
			label := StateLabel(step)
			got, err := Parse(label)
			if err != nil {
				t.Fatalf("Parse(%q): %v", label, err)
			}
			if got.Kind != KindState {
				t.Errorf("Kind = %s, want state", got.Kind)
			}
			if got.Step != step {
				t.Errorf("Step = %q, want %q", got.Step, step)
			}
		})
	}
}

// TestParse_ApplyingRoundTrip: generar `che:state:applying:<step>` y
// parsearlo de vuelta. Crucial porque el prefijo applying es superset
// del state — si el orden de checks en Parse se invierte, este test
// rompe primero.
func TestParse_ApplyingRoundTrip(t *testing.T) {
	cases := []string{"idea", "explore", "validate_issue", "execute", "validate_pr", "close"}
	for _, step := range cases {
		t.Run(step, func(t *testing.T) {
			label := ApplyingLabel(step)
			got, err := Parse(label)
			if err != nil {
				t.Fatalf("Parse(%q): %v", label, err)
			}
			if got.Kind != KindApplying {
				t.Errorf("Kind = %s, want applying (Parse priorizó PrefixState antes que PrefixApplying?)", got.Kind)
			}
			if got.Step != step {
				t.Errorf("Step = %q, want %q", got.Step, step)
			}
		})
	}
}

// TestParse_LockRoundTrip: generar un lock con LockLabelAt (inputs fijos)
// y parsearlo de vuelta debe recuperar timestamp, pid y host exactos.
func TestParse_LockRoundTrip(t *testing.T) {
	// Truncar a nanosegundo: time.Unix(0, nanos) recupera con precisión
	// nano pero el input puede tener monotonic clock que se pierde en el
	// round-trip. Usar UnixNano explícitamente.
	when := time.Unix(0, time.Now().UnixNano())
	label := LockLabelAt(when, 4242, "build-host-01")

	got, err := Parse(label)
	if err != nil {
		t.Fatalf("Parse(%q): %v", label, err)
	}
	if got.Kind != KindLock {
		t.Errorf("Kind = %s, want lock", got.Kind)
	}
	if !got.Timestamp.Equal(when) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, when)
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}
	if got.Host != "build-host-01" {
		t.Errorf("Host = %q, want %q", got.Host, "build-host-01")
	}
}

// TestParse_LockHostWithDashes blinda el caso donde el hostname tiene
// guiones. El parser separa <pid>-<host> por el PRIMER `-` (pid es
// numérico) — un host como "my-machine-name" debe preservarse entero.
func TestParse_LockHostWithDashes(t *testing.T) {
	when := time.Unix(0, 1700000000000000000)
	label := LockLabelAt(when, 99, "my-machine-name")

	got, err := Parse(label)
	if err != nil {
		t.Fatalf("Parse(%q): %v", label, err)
	}
	if got.Host != "my-machine-name" {
		t.Errorf("Host = %q, want %q (parser cortó por el wrong dash)", got.Host, "my-machine-name")
	}
	if got.PID != 99 {
		t.Errorf("PID = %d, want 99", got.PID)
	}
}

// TestLockLabel_DistinctOnConsecutiveCalls cubre la garantía pedida en
// el scope: dos generaciones consecutivas devuelven labels distintos
// (timestamp avanza). UnixNano evita necesitar sleep en el test.
func TestLockLabel_DistinctOnConsecutiveCalls(t *testing.T) {
	a := LockLabel()
	b := LockLabel()
	if a == b {
		t.Errorf("LockLabel() returned identical labels on consecutive calls\n  a: %s\n  b: %s", a, b)
	}
	// Sanity: ambos deben parsear como locks (no estamos generando otra
	// cosa por accidente).
	for i, lab := range []string{a, b} {
		p, err := Parse(lab)
		if err != nil {
			t.Errorf("Parse(call %d, %q): %v", i, lab, err)
			continue
		}
		if p.Kind != KindLock {
			t.Errorf("call %d: kind = %s, want lock", i, p.Kind)
		}
	}
}

// TestLockLabel_UsesCurrentProcess verifica que LockLabel() captura el PID
// del proceso actual. Es la diferencia entre LockLabel y LockLabelAt: el
// "real" tiene que identificar quién está sosteniendo el lock.
func TestLockLabel_UsesCurrentProcess(t *testing.T) {
	label := LockLabel()
	p, err := Parse(label)
	if err != nil {
		t.Fatalf("Parse(%q): %v", label, err)
	}
	if p.PID <= 0 {
		t.Errorf("PID = %d, want >0 (os.Getpid())", p.PID)
	}
	if p.Host == "" {
		t.Errorf("Host empty — LockLabel debería caer al hostname real o al fallback unknown-host")
	}
	// El timestamp debe estar dentro de una ventana razonable (último
	// minuto), descartando bugs como pasar 0 sin querer.
	delta := time.Since(p.Timestamp)
	if delta < 0 || delta > time.Minute {
		t.Errorf("Timestamp delta = %v, want within [0, 1m] (lock recién generado)", delta)
	}
}

// TestParse_NotPipelineLabel: cualquier string que no tenga uno de los 3
// prefijos canónicos debe devolver ErrNotPipelineLabel exactly. Cubre el
// caso "el caller le pasa un label random del repo" (ej. ct:plan,
// validated:approve, bug, etc.).
func TestParse_NotPipelineLabel(t *testing.T) {
	cases := []string{
		"",
		"bug",
		"ct:plan",
		"validated:approve",
		"che:plan",          // label viejo de internal/labels
		"che:idea",          // ídem
		"che:locked",        // ídem (mutex viejo)
		"che:state",         // sin prefijo completo (no termina con `:`)
		"che:state-foo",     // similar pero con guion
		"che:lock",          // sin sufijo
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := Parse(in)
			if !errors.Is(err, ErrNotPipelineLabel) {
				t.Errorf("Parse(%q) error = %v, want ErrNotPipelineLabel", in, err)
			}
		})
	}
}

// TestParse_MalformedKnownPrefix: labels que tienen un prefijo conocido
// pero el resto está roto NO deben devolver ErrNotPipelineLabel — son
// nuestros pero malformados. Importante para drift detection: queremos
// distinguir "no es mío" de "es mío y está corrupto en el repo".
func TestParse_MalformedKnownPrefix(t *testing.T) {
	cases := []struct {
		name  string
		label string
	}{
		{"empty state step", "che:state:"},
		{"empty applying step", "che:state:applying:"},
		{"lock without colon", "che:lock:abc"},
		{"lock empty pid-host", "che:lock:123:"},
		{"lock non-numeric ts", "che:lock:notanumber:99-host"},
		{"lock without dash in pid-host", "che:lock:123:nohostpart"},
		{"lock non-numeric pid", "che:lock:123:abc-host"},
		{"lock empty host", "che:lock:123:99-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.label)
			if err == nil {
				t.Fatalf("Parse(%q) no error, want malformed error", c.label)
			}
			if errors.Is(err, ErrNotPipelineLabel) {
				t.Errorf("Parse(%q) = ErrNotPipelineLabel, want a malformed-label error", c.label)
			}
			if !strings.Contains(err.Error(), "pipelinelabels:") {
				t.Errorf("Parse(%q) error = %q, want prefixed with 'pipelinelabels:'", c.label, err.Error())
			}
		})
	}
}

// TestKindString cubre el helper String() de Kind para que mensajes de
// error/log sean legibles. Trivial pero evita un drift silencioso si
// alguien agrega un Kind nuevo y olvida actualizar el switch.
func TestKindString(t *testing.T) {
	cases := map[Kind]string{
		KindUnknown:  "unknown",
		KindState:    "state",
		KindApplying: "applying",
		KindLock:     "lock",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// TestLockLabelAt_FallbackHost: pasar host vacío a LockLabelAt no debe
// generar un label malformado — el helper inyecta `unknown-host` como
// hace LockLabel cuando os.Hostname falla.
func TestLockLabelAt_FallbackHost(t *testing.T) {
	label := LockLabelAt(time.Unix(0, 1), 1, "")
	p, err := Parse(label)
	if err != nil {
		t.Fatalf("Parse(%q): %v", label, err)
	}
	if p.Host != "unknown-host" {
		t.Errorf("Host = %q, want unknown-host", p.Host)
	}
}
