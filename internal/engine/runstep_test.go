package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingInvoker simula agentes que tardan en responder. Permite verificar
// cancelación parcial: el invoker espera un signal antes de devolver, salvo
// que el ctx se cancele primero.
type blockingInvoker struct {
	// fixed: respuesta inmediata por agente. Si presente, devuelve el
	// marker sin bloquear (útil para agentes "fast" que dispararán la
	// decisión del aggregator).
	fixed map[string]string

	// release: si el agente está acá, espera hasta que el test cierre el
	// channel ANTES de devolver (simula un agente "slow"). Si el ctx se
	// cancela durante la espera, devuelve ctx.Err() — esto refleja el
	// comportamiento de internal/agent.Run con ctx propagado al child.
	release map[string]<-chan struct{}

	// invoked / cancelled tracking, en orden de invocación.
	mu        sync.Mutex
	started   []string
	cancelled []string
	completed []string
}

func newBlockingInvoker() *blockingInvoker {
	return &blockingInvoker{
		fixed:   map[string]string{},
		release: map[string]<-chan struct{}{},
	}
}

func (b *blockingInvoker) Invoke(ctx context.Context, agent string, _ string) (string, OutputFormat, error) {
	b.mu.Lock()
	b.started = append(b.started, agent)
	b.mu.Unlock()

	if out, ok := b.fixed[agent]; ok {
		b.mu.Lock()
		b.completed = append(b.completed, agent)
		b.mu.Unlock()
		return out, FormatText, nil
	}

	if rel, ok := b.release[agent]; ok {
		select {
		case <-rel:
			b.mu.Lock()
			b.completed = append(b.completed, agent)
			b.mu.Unlock()
			return "[next]", FormatText, nil
		case <-ctx.Done():
			b.mu.Lock()
			b.cancelled = append(b.cancelled, agent)
			b.mu.Unlock()
			return "", FormatText, ctx.Err()
		}
	}

	// Sin fixed ni release configurado: respuesta default.
	return "[next]", FormatText, nil
}

// ---------- runStep: comportamiento básico ----------

func TestRunStep_SingleAgenteEquivalente(t *testing.T) {
	// Con 1 agente, runStep debe producir el mismo outcome que el motor
	// pre-PR5c (un solo result, marker preservado).
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "hola\n[next]", FormatText, nil
	})
	step := Step{Name: "explore", Agents: []string{"a"}}
	out := runStep(context.Background(), step, inv, "input")
	if out.Marker.Kind != MarkerNext {
		t.Errorf("Marker.Kind=%v want next", out.Marker.Kind)
	}
	if len(out.Results) != 1 {
		t.Fatalf("Results=%d want 1", len(out.Results))
	}
	if out.Results[0].Agent != "a" {
		t.Errorf("Result.Agent=%q want a", out.Results[0].Agent)
	}
}

func TestRunStep_TresAgentesEnParaleloMajority(t *testing.T) {
	// 3 agentes, todos [next] → majority next. Verifica que los 3 se
	// invocan (paralelo, no secuencial cortando al primero).
	var startedSimul int32
	var maxConcurrent int32
	inv := newFakeInvokerWith(func(agent string, _ int) (string, OutputFormat, error) {
		// Subimos contador, esperamos un poco para que los 3 coincidan.
		now := atomic.AddInt32(&startedSimul, 1)
		for {
			cur := atomic.LoadInt32(&maxConcurrent)
			if now <= cur || atomic.CompareAndSwapInt32(&maxConcurrent, cur, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&startedSimul, -1)
		return "[next]", FormatText, nil
	})
	step := Step{Name: "validate", Agents: []string{"a", "b", "c"}, Aggregator: AggMajority}
	out := runStep(context.Background(), step, inv, "")
	if out.Marker.Kind != MarkerNext {
		t.Errorf("Marker.Kind=%v want next", out.Marker.Kind)
	}
	if atomic.LoadInt32(&maxConcurrent) < 2 {
		t.Errorf("maxConcurrent=%d want >=2 (esperaba paralelismo real)", maxConcurrent)
	}
}

// ---------- runStep: cancelación parcial ----------

func TestRunStep_FirstBlockerCancelaResto(t *testing.T) {
	// 3 agentes, A devuelve [stop] al toque, B y C bloqueados. El
	// aggregator first_blocker decide en cuanto ve el [stop] y debe
	// cancelar B y C — el ctx propaga al Invoke y devuelve error de ctx,
	// que runStep marca como Cancelled (NO error).
	bRel := make(chan struct{})
	cRel := make(chan struct{})
	inv := newBlockingInvoker()
	inv.fixed["a"] = "[stop]"
	inv.release["b"] = bRel
	inv.release["c"] = cRel

	step := Step{Name: "validate", Agents: []string{"a", "b", "c"}, Aggregator: AggFirstBlocker}

	done := make(chan stepOutcome, 1)
	go func() {
		done <- runStep(context.Background(), step, inv, "")
	}()

	select {
	case out := <-done:
		// runStep retornó. Verificamos que el marker sea [stop] y que
		// b y c estén marcados Cancelled.
		if out.Marker.Kind != MarkerStop {
			t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
		}
		// Buscar b y c en results: deben estar Cancelled=true (o no
		// estar todavía si hubo race). Vamos a esperar al cleanup.
		var cancelledCount int
		for _, r := range out.Results {
			if r.Cancelled {
				cancelledCount++
				if r.Err != nil {
					t.Errorf("agente %q cancelled pero también con Err=%v; debería ser solo Cancelled", r.Agent, r.Err)
				}
			}
		}
		if cancelledCount < 2 {
			t.Errorf("Cancelled count=%d want 2 (b y c); results=%+v", cancelledCount, out.Results)
		}
	case <-time.After(2 * time.Second):
		// No retornó: debugging info.
		close(bRel)
		close(cRel)
		t.Fatal("runStep no retornó en 2s — la cancelación no propagó")
	}

	// Cleanup channels (idempotent close).
	defer func() { recover() }()
	close(bRel)
	close(cRel)
}

func TestRunStep_MajorityStopShortCircuit(t *testing.T) {
	// majority + 3 agentes: A=[stop] (rápido), B y C bloqueados. El
	// aggregator majority decide [stop] al ver el primer stop. Verifica
	// que B y C NO completaron (fueron cancelados).
	bRel := make(chan struct{})
	cRel := make(chan struct{})
	inv := newBlockingInvoker()
	inv.fixed["a"] = "[stop]"
	inv.release["b"] = bRel
	inv.release["c"] = cRel

	step := Step{Name: "s", Agents: []string{"a", "b", "c"}, Aggregator: AggMajority}
	out := runStep(context.Background(), step, inv, "")

	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
	}

	inv.mu.Lock()
	completed := append([]string{}, inv.completed...)
	cancelled := append([]string{}, inv.cancelled...)
	inv.mu.Unlock()

	if len(completed) != 1 || completed[0] != "a" {
		t.Errorf("completed=%v want [a] (b y c deberían haber sido cancelados)", completed)
	}
	if len(cancelled) != 2 {
		t.Errorf("cancelled=%v want 2 agentes (b y c)", cancelled)
	}

	// Cleanup (defensivo — los release nunca se cierran porque los goroutines
	// ya retornaron por ctx-cancel).
	defer func() { recover() }()
	close(bRel)
	close(cRel)
}

func TestRunStep_UnanimousDivergenciaCancelaResto(t *testing.T) {
	// unanimous con A=[next] B=[stop] C=bloqueado. Apenas llegan A y B
	// (divergencia detectada), el aggregator decide [stop] y cancela C.
	cRel := make(chan struct{})
	inv := newBlockingInvoker()
	inv.fixed["a"] = "[next]"
	inv.fixed["b"] = "[stop]"
	inv.release["c"] = cRel

	step := Step{Name: "s", Agents: []string{"a", "b", "c"}, Aggregator: AggUnanimous}
	out := runStep(context.Background(), step, inv, "")

	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
	}
	// C debería estar Cancelled.
	var cFound bool
	for _, r := range out.Results {
		if r.Agent == "c" {
			cFound = true
			if !r.Cancelled {
				t.Errorf("agent c esperaba Cancelled=true; got %+v", r)
			}
		}
	}
	if !cFound {
		t.Error("agent c no apareció en Results — debería estar al menos como Cancelled")
	}

	defer func() { recover() }()
	close(cRel)
}

// ---------- runStep: aggregator default + multi-instance ----------

func TestRunStep_AggregatorDefaultEsMajority(t *testing.T) {
	// Step.Aggregator vacío → majority por default (PRD §3.d).
	// 3 agentes, 3 [next] → next. Si el default fuera otro distinto, esto
	// fallaría con first_blocker (que también devuelve next, OK) pero con
	// unanimous fallaría también. Hacemos un caso que SÍ distingue:
	// 2 [next] + 1 [stop] → majority dice stop (short-circuit), unanimous
	// también dice stop (divergencia), first_blocker dice stop. No hay
	// caso que diferencie majority del resto sin majority de N>3, así que
	// confiamos en aggregator_test.go para la lógica y acá sólo verificamos
	// que el default ALGÚN aggregator se aplica (no nil panic).
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "[next]", FormatText, nil
	})
	step := Step{Name: "s", Agents: []string{"a", "b", "c"}}
	out := runStep(context.Background(), step, inv, "")
	if out.Marker.Kind != MarkerNext {
		t.Errorf("Marker.Kind=%v want next (default aggregator debería ser majority)", out.Marker.Kind)
	}
}

func TestRunStep_AgenteRepetidoEsNInstancias(t *testing.T) {
	// PRD §3.a: "mismo agente repetido = N instancias". El invoker recibe
	// el mismo nombre N veces y debe tratarlas como invocaciones
	// independientes. Verificamos que el invoker se llamó 3 veces para
	// el mismo agente.
	var callCount int32
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		atomic.AddInt32(&callCount, 1)
		return "[next]", FormatText, nil
	})
	step := Step{Name: "s", Agents: []string{"opus", "opus", "opus"}, Aggregator: AggMajority}
	out := runStep(context.Background(), step, inv, "")
	if out.Marker.Kind != MarkerNext {
		t.Errorf("Marker.Kind=%v want next", out.Marker.Kind)
	}
	if atomic.LoadInt32(&callCount) != 3 {
		t.Errorf("invoker calls=%d want 3", callCount)
	}
	if len(out.Results) != 3 {
		t.Errorf("Results=%d want 3", len(out.Results))
	}
}

// ---------- runStep: errores técnicos ----------

func TestRunStep_TodosErrorTecnico_StopConTechErr(t *testing.T) {
	// 3 agentes, todos errorean. Aggregator majority short-circuitea con
	// el primer error (cuenta como [stop]). El outcome propaga
	// TechnicalError para que el motor mapee a StopReasonTechnicalError.
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "", FormatText, errExitNonZero
	})
	step := Step{Name: "s", Agents: []string{"a", "b", "c"}, Aggregator: AggMajority}
	out := runStep(context.Background(), step, inv, "")
	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
	}
	if out.TechnicalError == nil {
		t.Error("TechnicalError nil; want propagated err")
	}
}

func TestRunStep_CtxYaCancelado(t *testing.T) {
	// Si el ctx parent ya está cancelado al entrar, runStep retorna [stop]
	// inmediato sin invocar.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var called bool
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		called = true
		return "[next]", FormatText, nil
	})
	step := Step{Name: "s", Agents: []string{"a", "b"}, Aggregator: AggMajority}
	out := runStep(ctx, step, inv, "")
	if out.Marker.Kind != MarkerStop {
		t.Errorf("Marker.Kind=%v want stop", out.Marker.Kind)
	}
	if called {
		t.Error("invoker no debería haber sido llamado con ctx cancelado")
	}
	if out.TechnicalError == nil {
		t.Error("TechnicalError nil; want ctx.Err()")
	}
}

// ---------- integración con RunPipeline ----------

func TestRunPipeline_StepMultiAgenteIntegracion(t *testing.T) {
	// Pipeline con un step de 3 agentes. majority resuelve [stop] al
	// primer stop. Verifica que el motor wirea bien runStep + StepRun
	// trae AgentResults + AggregatorReason.
	bRel := make(chan struct{})
	cRel := make(chan struct{})
	inv := newBlockingInvoker()
	inv.fixed["a"] = "[stop]"
	inv.release["b"] = bRel
	inv.release["c"] = cRel

	pipe := Pipeline{Steps: []Step{
		{Name: "validate", Agents: []string{"a", "b", "c"}, Aggregator: AggMajority},
	}}
	run, err := RunPipeline(context.Background(), pipe, inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false; expected true")
	}
	if run.StopReason != StopReasonAgentMarker {
		t.Errorf("StopReason=%q want %q", run.StopReason, StopReasonAgentMarker)
	}
	if len(run.Steps) != 1 {
		t.Fatalf("Steps=%d want 1", len(run.Steps))
	}
	sr := run.Steps[0]
	if sr.Resolved != "aggregator" {
		t.Errorf("Resolved=%q want aggregator", sr.Resolved)
	}
	if sr.AggregatorReason == "" {
		t.Error("AggregatorReason vacía; esperaba detalle del aggregator")
	}
	if len(sr.AgentResults) < 1 {
		t.Errorf("AgentResults=%d want >=1", len(sr.AgentResults))
	}
	// Agent debería estar vacío en multi-agente (StepRun.AgentResults trae
	// el detalle).
	if sr.Agent != "" {
		t.Errorf("Agent=%q want empty (multi-agent)", sr.Agent)
	}

	defer func() { recover() }()
	close(bRel)
	close(cRel)
}

func TestRunPipeline_StepMultiAgenteErrorTecnicoMapeaATechReason(t *testing.T) {
	// Si el aggregator decide [stop] basado en errores técnicos, el motor
	// debe usar StopReasonTechnicalError (no StopReasonAgentMarker).
	inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
		return "", FormatText, errExitNonZero
	})
	pipe := Pipeline{Steps: []Step{
		{Name: "s", Agents: []string{"a", "b"}, Aggregator: AggMajority},
	}}
	run, err := RunPipeline(context.Background(), pipe, inv, Options{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !run.Stopped {
		t.Fatalf("Stopped=false")
	}
	if run.StopReason != StopReasonTechnicalError {
		t.Errorf("StopReason=%q want %q", run.StopReason, StopReasonTechnicalError)
	}
}

func TestRunStep_SingleAgenteIgnoraAggregator(t *testing.T) {
	// Con 1 agente, runStep debe producir el mismo outcome con cualquiera
	// de los 3 presets (PRD §3.d: el aggregator se ignora en single-agent).
	// Iteramos los 3 kinds + el default ("") y verificamos que el marker
	// final sea idéntico.
	for _, kind := range []AggregatorKind{"", AggMajority, AggUnanimous, AggFirstBlocker} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			inv := newFakeInvoker(func(agent string, _ int) (string, OutputFormat, error) {
				return "hola\n[next]", FormatText, nil
			})
			step := Step{Name: "s", Agents: []string{"a"}, Aggregator: kind}
			out := runStep(context.Background(), step, inv, "input")
			if out.Marker.Kind != MarkerNext {
				t.Errorf("kind=%q Marker.Kind=%v want next", kind, out.Marker.Kind)
			}
			if len(out.Results) != 1 {
				t.Fatalf("kind=%q Results=%d want 1", kind, len(out.Results))
			}
			if out.Results[0].Agent != "a" {
				t.Errorf("kind=%q Result.Agent=%q want a", kind, out.Results[0].Agent)
			}
			if out.TechnicalError != nil {
				t.Errorf("kind=%q TechnicalError=%v want nil", kind, out.TechnicalError)
			}
		})
	}
}

// newFakeInvokerWith: mismo shape que newFakeInvoker pero sin tracking
// (los tests de paralelismo usan atómicos externos).
func newFakeInvokerWith(fn func(string, int) (string, OutputFormat, error)) *fakeInvoker {
	return newFakeInvoker(fn)
}
