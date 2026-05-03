// Constantes "v2" del modelo de labels derivado del pipeline declarativo
// (PRD §6.c). Son la fuente de verdad runtime de la máquina de estados
// post-PR6c — el modelo viejo (`che:idea` … `che:closed`) ya no es
// runtime, sólo lo reconoce el subcomando `migrate-labels-v2` y los
// guards `rejectV1Labels` de los flows como input legacy.
//
// El test `internal/labels.TestNoHardcodedLabelsOutsideThisPackage`
// prohíbe los literales v2 fuera de este paquete y `internal/labels` —
// los callers deben usar `pipelinelabels.State*`.
//
// Mapeo desde el modelo viejo (referencia histórica para el subcomando
// `migrate-labels-v2`; PRD §6.c):
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
// del pipeline, no del label. Mantenemos el mapeo 1:1 con los 9 estados
// viejos para no cambiar semántica; refinar a steps separados
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
	// `che:idea` del modelo legacy. Forma: `che:state:idea`.
	StateIdea = StateLabel(StepIdea)

	// StateApplyingExplore es el lock optimista del step `explore`. Mapea al
	// viejo `che:planning` del modelo legacy (estado transient mientras corre
	// explore). Forma: `che:state:applying:explore`.
	StateApplyingExplore = ApplyingLabel(StepExplore)

	// StateExplore es el estado terminal del step `explore`. Mapea al viejo
	// `che:plan` del modelo legacy (explore terminó OK; existe un plan listo
	// para ejecutar). Forma: `che:state:explore`.
	StateExplore = StateLabel(StepExplore)

	// StateApplyingExecute es el lock optimista del step `execute`. Mapea
	// al viejo `che:executing`. Forma: `che:state:applying:execute`.
	StateApplyingExecute = ApplyingLabel(StepExecute)

	// StateExecute es el estado terminal del step `execute`. Mapea al
	// viejo `che:executed` (execute terminó OK; PR abierto).
	// Forma: `che:state:execute`.
	StateExecute = StateLabel(StepExecute)

	// StateApplyingValidatePR es el lock optimista del step `validate_pr`.
	// Mapea al viejo `che:validating`. En el modelo v2 no distinguimos
	// validate-de-plan vs. validate-de-PR — colapsan a un solo step
	// `validate_pr`; refinar es follow-up.
	// Forma: `che:state:applying:validate_pr`.
	StateApplyingValidatePR = ApplyingLabel(StepValidatePR)

	// StateValidatePR es el estado terminal del step `validate_pr`. Mapea
	// al viejo `che:validated`. Forma: `che:state:validate_pr`.
	StateValidatePR = StateLabel(StepValidatePR)

	// StateApplyingClose es el lock optimista del step `close`. Mapea al
	// viejo `che:closing`. Forma: `che:state:applying:close`.
	StateApplyingClose = ApplyingLabel(StepClose)

	// StateClose es el estado terminal del step `close`. Mapea al viejo
	// `che:closed`. Forma: `che:state:close`.
	StateClose = StateLabel(StepClose)
)
