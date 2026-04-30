package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chichex/che/internal/agent"
	"github.com/chichex/che/internal/agentregistry"
	"github.com/chichex/che/internal/engine"
	"github.com/chichex/che/internal/pipeline"
	"github.com/spf13/cobra"
)

var (
	runFromStep string
	runInput    string
)

var runCmd = &cobra.Command{
	Use:   "run [pipeline]",
	Short: "ejecuta un pipeline (entry agent + steps + markers)",
	Long: `run resuelve un pipeline (jerarquía: arg posicional > config.default >
built-in, PRD §7.b) y lo ejecuta a través del engine: corre el entry
agent (si hay), parsea su marker, y dispara los steps en orden hasta
[stop] o hasta el último step.

Override manual del entry: usá '--from <step>' para bypassear el entry
y arrancar directamente desde el step pedido (PRD §5.c). Útil para
re-correr una etapa puntual sin volver a pasar por el validador del
input.

Esta v1 sólo invoca a los 3 agentes built-in (claude-opus/sonnet/haiku)
vía el binario 'claude'. Pipelines que referencien agentes custom
emitirán [stop] técnico — el wiring de subagents custom de Claude Code
llega en un follow-up.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		root, err := repoRootForPipeline()
		if err != nil {
			return err
		}
		mgr, err := pipeline.NewManager(root)
		if err != nil {
			return fmt.Errorf("init pipeline manager: %s", formatLoadError(err))
		}
		flag := ""
		if len(args) == 1 {
			flag = args[0]
		}
		// Live invoker: usa internal/agent.Run para los built-ins. Tests
		// pasan un fake via runPipelineRun directo.
		inv := newLiveInvoker()
		return runPipelineRun(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), mgr, inv, flag, runFromStep, runInput)
	},
}

func init() {
	runCmd.Flags().StringVar(&runFromStep, "from", "",
		"step desde el que arrancar — bypassa el entry agent (PRD §5.c)")
	runCmd.Flags().StringVar(&runInput, "input", "",
		"input inicial del pipeline (texto libre que recibe el entry o el primer step)")
	rootCmd.AddCommand(runCmd)
}

// runPipelineRun es la función testeable: resuelve el pipeline, lo
// convierte al shape del engine, dispara la ejecución y formatea el
// outcome. Separar el wiring del invocador concreto permite que los
// tests inyecten un fake sin spawnear el CLI de claude.
func runPipelineRun(ctx context.Context, out, errOut io.Writer, mgr *pipeline.Manager, inv engine.Invoker, pipelineFlag, fromStep, input string) error {
	r, err := mgr.Resolve(pipelineFlag)
	if err != nil {
		return fmt.Errorf("%s", formatLoadError(err))
	}

	src := r.Path
	if src == "" {
		src = "<built-in>"
	}
	fmt.Fprintf(out, "pipeline: %s\n", r.Name)
	fmt.Fprintf(out, "source:   %s (%s)\n", r.Source, src)
	if fromStep != "" {
		fmt.Fprintf(out, "from:     %s (entry bypassed)\n", fromStep)
	}
	fmt.Fprintln(out, "")

	// Convertir pipeline.Pipeline → engine.Pipeline. v1 ignora el
	// Aggregator de Steps/Entry (PR5b/PR5d sólo invocan al primer
	// agente; PR5c follow-up wirea multi-agente sin tocar este mapeo).
	ep := toEnginePipeline(r.Pipeline)

	if ctx == nil {
		ctx = context.Background()
	}
	run, err := engine.RunPipeline(ctx, ep, inv, engine.Options{
		EntryStep: fromStep,
		Input:     input,
	})
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}

	writeRunSummary(out, run)

	// Exit semántico: stop técnico → 1; stop por agente o entry → 2;
	// completado ok → 0. Cobra lo traduce vía SilenceErrors+RunE.
	if run.Stopped {
		switch run.StopReason {
		case engine.StopReasonTechnicalError, engine.StopReasonEmptyPipeline,
			engine.StopReasonNoAgents, engine.StopReasonEntryNoAgents,
			engine.StopReasonUnknownStep:
			return fmt.Errorf("stop: %s — %s", run.StopReason, run.StopDetail)
		}
		// AgentMarker / EntryStop / LoopCap: no son errores técnicos,
		// son outcomes legítimos del pipeline. Exit 0 + mensaje en out.
	}
	return nil
}

// writeRunSummary imprime el outcome del run en formato humano (no
// JSON). El JSON estructurado puede sumarse con --format json en un
// follow-up.
func writeRunSummary(out io.Writer, run engine.Run) {
	if run.Entry != nil {
		fmt.Fprintf(out, "entry: agent=%s marker=%s resolved=%s",
			run.Entry.Agent, describeMarker(run.Entry.Marker), run.Entry.Resolved)
		if run.Entry.StartStep != "" {
			fmt.Fprintf(out, " start=%s", run.Entry.StartStep)
		}
		if run.Entry.Err != nil {
			fmt.Fprintf(out, " err=%v", run.Entry.Err)
		}
		fmt.Fprintln(out, "")
	}
	for i, s := range run.Steps {
		fmt.Fprintf(out, "step[%d]: %s agent=%s marker=%s resolved=%s",
			i, s.Step, s.Agent, describeMarker(s.Marker), s.Resolved)
		if s.Err != nil {
			fmt.Fprintf(out, " err=%v", s.Err)
		}
		fmt.Fprintln(out, "")
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "transitions: %d / %d\n", run.Transitions, engine.MaxTransitions)
	if run.Stopped {
		fmt.Fprintf(out, "stopped: %s — %s\n", run.StopReason, run.StopDetail)
	} else {
		fmt.Fprintln(out, "completed: pipeline ran to end")
	}
}

func describeMarker(m engine.Marker) string {
	switch m.Kind {
	case engine.MarkerGoto:
		return "[goto: " + m.Goto + "]"
	case engine.MarkerStop:
		return "[stop]"
	case engine.MarkerNext:
		return "[next]"
	default:
		return "[none]"
	}
}

// toEnginePipeline convierte el shape on-disk al shape del motor.
// El campo Aggregator (Steps y Entry) NO se mapea en v1 — PR5b/PR5d
// ignoran esa metadata por single-agent. El follow-up post-PR5c wirea
// el AggregatorKind acá sin cambiar callers.
func toEnginePipeline(p pipeline.Pipeline) engine.Pipeline {
	ep := engine.Pipeline{Steps: make([]engine.Step, 0, len(p.Steps))}
	for _, s := range p.Steps {
		ep.Steps = append(ep.Steps, engine.Step{
			Name:   s.Name,
			Agents: append([]string(nil), s.Agents...),
		})
	}
	if p.Entry != nil {
		ep.Entry = &engine.EntrySpec{
			Agents:     append([]string(nil), p.Entry.Agents...),
			Aggregator: engine.AggregatorKind(p.Entry.Aggregator),
		}
	}
	return ep
}

// liveInvoker mapea agentes built-in a `internal/agent.Run` (claude
// binary). Devuelve error técnico para agentes custom — esos los
// resuelve el wiring con Claude Code subagents en un follow-up.
type liveInvoker struct {
	registry *agentregistry.Registry
}

func newLiveInvoker() *liveInvoker {
	reg, regErrs := agentregistry.Discover(agentregistry.Options{IncludeBuiltins: true})
	for _, e := range regErrs {
		fmt.Fprintln(os.Stderr, "warn:", e.Error())
	}
	return &liveInvoker{registry: reg}
}

func (li *liveInvoker) Invoke(ctx context.Context, agentName string, input string) (string, engine.OutputFormat, error) {
	a, ok := li.registry.Get(agentName)
	if !ok {
		return "", engine.FormatText, fmt.Errorf("agent %q not found in registry", agentName)
	}
	// v1: sólo soportamos los 3 built-ins. Custom agents requieren
	// invocar claude con --agent <name> o equivalente — fuera del
	// scope de PR5d.
	if a.Source != agentregistry.SourceBuiltin {
		return "", engine.FormatText, fmt.Errorf("agent %q (source=%s) requires custom-agent wiring (follow-up); v1 sólo soporta built-ins", agentName, a.Source)
	}
	// Map "claude-opus" → opus enum.
	model := strings.TrimPrefix(a.Name, "claude-")
	ag, err := agent.ParseAgent(model)
	if err != nil {
		return "", engine.FormatText, fmt.Errorf("agent %q: %w", agentName, err)
	}
	res, err := agent.Run(ag, input, agent.RunOpts{
		Ctx:    ctx,
		Format: agent.OutputText,
	})
	if err != nil {
		// El motor mapea cualquier error técnico a [stop]; devolvemos
		// el stdout acumulado por si el caller quiere loguearlo.
		return res.Stdout, engine.FormatText, err
	}
	return res.Stdout, engine.FormatText, nil
}

// Asegurar que liveInvoker implementa engine.Invoker en compile-time.
var _ engine.Invoker = (*liveInvoker)(nil)
