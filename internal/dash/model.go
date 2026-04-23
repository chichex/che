// Package dash — modelo de datos del dashboard. Define los tipos `Entity`,
// `LogRun`, `LogEntry` que representan lo que se muestra en una card / drawer,
// junto con el dispatcher `Column` que decide en qué columna del board cae una
// entidad según sus labels reales (ct:plan, status:*, plan-validated:*,
// validated:*, che:locked) más el estado efímero de los flows en curso
// (RunningFlow, RunIter, RunMax).
//
// Step 2: estos tipos los popula `mock.go` con datos hardcodeados; en pasos
// posteriores los completará un poller que lea labels de gh api.
package dash

// EntityKind distingue una entidad "issue suelto" (sin PR aún) de una
// "fusionada" issue+PR (vinculados via close-keywords). El dispatcher de
// columna usa esto para separar lo que está en el funnel pre-execute de lo
// que ya tiene PR abierto.
type EntityKind int

const (
	// KindIssue: solo hay un issue, todavía no se abrió el PR. Cubre las
	// columnas backlog / exploring / plan.
	KindIssue EntityKind = iota
	// KindFused: issue + PR vinculados (close-keywords). El render usa el
	// número del PR como ref principal y deja el del issue en breadcrumb.
	// Cubre executing / validating / approved.
	KindFused
)

// Entity es la unidad de datos del board y del drawer. Reúne campos
// esenciales para la card (Kind, IssueNumber, IssueTitle, PRNumber, PRTitle,
// Type, Size, Status, verdicts, Locked, RunningFlow) y campos extra solo
// relevantes en el drawer (LastAction, NextAction, LoopSpec, LogRuns).
//
// Los strings de labels (Type, Size, Status, PlanVerdict, PRVerdict) usan el
// valor "corto" (sin el prefijo `type:`, `size:`, etc.) para mantener el
// modelo agnóstico de cómo se renderiza el label en el chip. La capa de
// presentación arma `type:feature` cuando hace falta.
type Entity struct {
	Kind        EntityKind
	IssueNumber int
	IssueTitle  string
	PRNumber    int    // 0 si issue-only
	PRTitle     string // vacío si issue-only
	Branch      string // ej "feat/dash-fusion"
	SHA         string // short SHA

	Type        string // "feature" | "fix" | "mejora" | "ux" — vacío = desconocido
	Size        string // "xs" | "s" | "m" | "l" | "xl" — vacío = desconocido
	Status      string // "idea" | "plan" | "executing" | "executed" | "closed"
	PlanVerdict string // "approve" | "changes-requested" | "needs-human" — vacío = no validado
	PRVerdict   string // idem, solo aplica si KindFused
	Locked      bool   // che:locked

	RunningFlow string // "explore" | "execute" | "iterate" | "validate" — vacío = idle
	RunIter     int    // iteración actual (1-based)
	RunMax      int    // max iteraciones del loop

	ChecksOK      int
	ChecksPending int
	ChecksFail    int

	// Drawer-only.
	LastAction string   // "iterate #3 → validate (hace 42s)"
	NextAction string   // "validate en ~18s"
	LoopSpec   string   // "iterate ↔ validate · run 3/10 · stop at approve"
	LogRuns    []LogRun // grupos de entries por run (más viejo primero)
	// IssueBody es el body (markdown) del issue original. Solo se muestra en
	// el tab "Issue" del drawer para entidades fused (contexto histórico
	// sobre por qué se abrió el PR) y debajo del drawer en issue-only. Vacío
	// si el issue no tiene descripción.
	IssueBody string
}

// LogRun agrupa logs de una iteración del loop. El renderer pinta un separador
// ("── run 2 · validate · verdict: ...") antes de las entries.
type LogRun struct {
	Label   string
	Entries []LogEntry
}

// LogEntry es una línea del stream. Class controla el color CSS aplicado al
// texto (info|tool|ok|warn|err); vacío = texto plano.
type LogEntry struct {
	Time  string
	Class string
	Text  string
}

// Column resuelve a qué columna del Kanban va la entidad. Las reglas siguen
// el funnel real de che: lo no fusionado vive en backlog/exploring/plan según
// status + flow corriendo; lo fusionado vive en executing/validating/approved
// según el verdict del PR.
//
// Reglas en orden (la primera que matchea gana):
//  1. Fusionada con PRVerdict=approve → "approved".
//  2. Fusionada (cualquier otro o sin verdict) → "validating".
//     Excepción: si Status=="executing", cae a "executing" (todavía no llegó al primer validate).
//  3. Status=="executing" → "executing" (aplica a fused y a issue-only por defensa).
//  4. Status=="plan" → "plan".
//  5. Status=="idea" + RunningFlow=="explore" → "exploring".
//  6. Default (incluye Status=="idea" idle, status vacío, status raro) → "backlog".
func (e Entity) Column() string {
	if e.Kind == KindFused {
		if e.Status == "executing" {
			return "executing"
		}
		if e.PRVerdict == "approve" {
			return "approved"
		}
		return "validating"
	}
	switch e.Status {
	case "executing":
		return "executing"
	case "plan":
		return "plan"
	case "idea":
		if e.RunningFlow == "explore" {
			return "exploring"
		}
		return "backlog"
	default:
		return "backlog"
	}
}
