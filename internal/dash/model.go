// Package dash — modelo de datos del dashboard. Define los tipos `Entity`,
// `LogRun`, `LogEntry` que representan lo que se muestra en una card / drawer,
// junto con el dispatcher `Column` que decide en qué columna del board cae una
// entidad según su `Status` (mapeo 1-a-1 con los 9 estados che:* + default
// "idea" para issues sin estado raro).
//
// Step 2: estos tipos los popula `mock.go` con datos hardcodeados; en pasos
// posteriores los completará un poller que lea labels de gh api.
package dash

import "time"

// EntityKind distingue una entidad "issue suelto" (sin PR aún) de una
// "fusionada" issue+PR (vinculados via close-keywords). El dispatcher de
// columna ya NO usa Kind (solo Status decide la columna), pero el render
// sigue diferenciándolo: KindFused muestra ref dual #issue → !PR, KindIssue
// muestra solo #issue.
type EntityKind int

const (
	// KindIssue: solo hay un issue, todavía no se abrió el PR. Cubre las
	// columnas idea / planning / plan (pre-execute).
	KindIssue EntityKind = iota
	// KindFused: issue + PR vinculados (close-keywords). El render usa el
	// número del PR como ref principal y deja el del issue en breadcrumb.
	// Cubre executing / executed / validating / validated / closing / closed.
	// Validating y validated también pueden contener issues (validate de plan).
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

	Type string // "feature" | "fix" | "mejora" | "ux" — vacío = desconocido
	Size string // "xs" | "s" | "m" | "l" | "xl" — vacío = desconocido
	// Status es el sufijo del label che:* (sin prefijo). Valores válidos:
	// "idea", "planning", "plan", "executing", "executed", "validating",
	// "validated", "closing", "closed". Vacío o desconocido cae a "idea"
	// (default defensivo en Column()).
	Status      string
	PlanVerdict string // "approve" | "changes-requested" | "needs-human" — vacío = no validado
	PRVerdict   string // idem, solo aplica si KindFused
	Locked      bool   // che:locked

	RunningFlow string // "explore" | "execute" | "iterate" | "validate" | "close" — vacío = idle
	RunIter     int    // iteración actual (1-based)
	RunMax      int    // max iteraciones del loop
	// CapReached: el auto-loop dejó de dispatchar sobre este issue porque
	// rounds[id] ya alcanzó LoopCap. Señal visual para el humano: "no vas a
	// ver esto moverse solo, decidí algo". Se setea en overlayRunning solo
	// para entities en status loopable (plan / validated / executed) — en
	// closing/closed/idea/etc el cap es irrelevante.
	CapReached bool

	// CreatedAt se usa para priorizar el auto-loop (ver loop.go): más viejo
	// primero, así un item que lleva tiempo en el board no queda atrás del
	// recién creado. Para fused (issue + PR) guardamos la más reciente de
	// las dos fechas — si iteraste el PR hace poco, la entity baja en la
	// cola. Zero si el poller no pudo resolverla (mocks, fixtures viejos).
	CreatedAt time.Time

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

// Column resuelve a qué columna del Kanban va la entidad. Mapeo 1-a-1 con
// `Status`: cada estado che:* tiene su propia columna. La diferencia entre
// transient y terminal (planning/validating/closing vs plan/validated/closed)
// se refleja a nivel UI vía el badge "hot" (animación pulsante) que se calcula
// en groupByColumn al ver RunningFlow != "".
//
// Default: "idea" (cualquier issue sin status raro o vacío). Antes era
// "backlog" pero en el modelo de 9 estados ya no hay backlog separado de
// idea — todo issue gestionado por che arranca como idea.
func (e Entity) Column() string {
	switch e.Status {
	case "idea":
		return "idea"
	case "planning":
		return "planning"
	case "plan":
		return "plan"
	case "executing":
		return "executing"
	case "executed":
		return "executed"
	case "validating":
		return "validating"
	case "validated":
		return "validated"
	case "closing":
		return "closing"
	case "closed":
		return "closed"
	default:
		// Status vacío o desconocido → idea (defensa, evita que entidades
		// huérfanas desaparezcan del board).
		return "idea"
	}
}
