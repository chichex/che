// Constantes "v2" del modelo de labels derivado del pipeline declarativo
// (PRD §6.c). Forman el mapeo 1:1 entre los 9 estados viejos del paquete
// `internal/labels` (`che:idea` … `che:closed`) y los nuevos `che:state:*`
// + `che:state:applying:*`.
//
// Existen para que la migración flow por flow del PR6b pueda referirse a
// los estados v2 con nombres ergonómicos (StateIdea, StateApplyingExplore,
// …) en lugar de inventar literales — el test
// `internal/labels.TestNoHardcodedLabelsOutsideThisPackage` ya prohíbe
// literales del modelo viejo, y queremos mantener la misma disciplina con
// el modelo nuevo. Cada caller que reemplace `labels.CheIdea` debe usar
// `pipelinelabels.StateIdea` en su lugar.
//
// Mapeo (PRD §6.c):
//
//	che:idea       → che:state:idea                       (StateIdea)
//	che:planning   → che:state:applying:explore           (StateApplyingExplore)
//	che:plan       → che:state:explore                    (StateExplore)
//	che:executing  → che:state:applying:execute           (StateApplyingExecute)
//	che:executed   → che:state:execute                    (StateExecute)
//	che:validating → che:state:applying:validate_pr       (StateApplyingValidatePR)
//	che:validated  → che:state:validate_pr                (StateValidatePR)
//	che:closing    → che:state:applying:close             (StateApplyingClose)
//	che:closed     → che:state:close                      (StateClose)
//
// Notar que la "validación del plan" del modelo viejo (validating/validated
// sobre un issue, vs. sobre un PR) colapsa en el modelo nuevo a un solo
// step `validate_pr` — el modelo nuevo deriva la entidad target del step
// del pipeline, no del label. Para PR6b mantenemos el mapeo 1:1 con los 9
// estados viejos para no cambiar semántica; refinar a steps separados
// (`validate_plan` vs. `validate_pr`) queda para PRs siguientes cuando el
// pipeline declarativo maneje ambos casos.
package pipelinelabels

// Nombres canónicos de los steps del modelo v2 derivado del pipeline
// declarativo. Exportados para que callers no inventen strings y para que
// test/golden puedan compararlos bit-perfect.
const (
	StepIdea       = "idea"
	StepExplore    = "explore"
	StepExecute    = "execute"
	StepValidatePR = "validate_pr"
	StepClose      = "close"
)

// Estados terminales (`che:state:<step>`) y aplicantes
// (`che:state:applying:<step>`) generados a partir de los nombres de step.
// Definidos como `var` (no `const`) porque se inicializan llamando a
// StateLabel/ApplyingLabel — las funciones del paquete son la fuente de
// verdad del prefijo.
var (
	// StateIdea es el estado terminal del step `idea`. Mapea al viejo
	// `labels.CheIdea`. Forma: `che:state:idea`.
	StateIdea = StateLabel(StepIdea)

	// StateApplyingExplore es el lock optimista del step `explore`. Mapea al
	// viejo `labels.ChePlanning` (estado transient mientras corre explore).
	// Forma: `che:state:applying:explore`.
	StateApplyingExplore = ApplyingLabel(StepExplore)

	// StateExplore es el estado terminal del step `explore`. Mapea al viejo
	// `labels.ChePlan` (explore terminó OK; existe un plan listo para
	// ejecutar). Forma: `che:state:explore`.
	StateExplore = StateLabel(StepExplore)

	// StateApplyingExecute es el lock optimista del step `execute`. Mapea
	// al viejo `labels.CheExecuting`. Forma: `che:state:applying:execute`.
	StateApplyingExecute = ApplyingLabel(StepExecute)

	// StateExecute es el estado terminal del step `execute`. Mapea al
	// viejo `labels.CheExecuted` (execute terminó OK; PR abierto).
	// Forma: `che:state:execute`.
	StateExecute = StateLabel(StepExecute)

	// StateApplyingValidatePR es el lock optimista del step `validate_pr`.
	// Mapea al viejo `labels.CheValidating`. En el modelo v2 no
	// distinguimos validate-de-plan vs. validate-de-PR — colapsan a un
	// solo step `validate_pr` para PR6b; refinar es follow-up.
	// Forma: `che:state:applying:validate_pr`.
	StateApplyingValidatePR = ApplyingLabel(StepValidatePR)

	// StateValidatePR es el estado terminal del step `validate_pr`. Mapea
	// al viejo `labels.CheValidated`. Forma: `che:state:validate_pr`.
	StateValidatePR = StateLabel(StepValidatePR)

	// StateApplyingClose es el lock optimista del step `close`. Mapea al
	// viejo `labels.CheClosing`. Forma: `che:state:applying:close`.
	StateApplyingClose = ApplyingLabel(StepClose)

	// StateClose es el estado terminal del step `close`. Mapea al viejo
	// `labels.CheClosed`. Forma: `che:state:close`.
	StateClose = StateLabel(StepClose)
)
