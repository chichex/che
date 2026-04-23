// Package dash — datos mock del Step 2, usados por MockSource (source.go).
//
// `mockEntities` devuelve una lista fija de entidades que cubre las 9
// columnas del board (idea, planning, plan, executing, executed, validating,
// validated, closing, closed) más algunos estados secundarios (locked,
// verdicts, checks).
//
// Se expone solo a través de MockSource — es internal (lowercase) para evitar
// que código externo lo consuma directo y se salte la abstracción de Source.
package dash

// mockEntities devuelve la lista de entidades de demo. Orden = orden con el
// que se muestran dentro de cada columna; el board agrupa por Column().
//
// Devuelve slices nuevos en cada llamada (las LogRuns también) para que
// modificar el resultado no afecte futuras llamadas — MockSource.Snapshot
// se llama desde handlers concurrentes y queremos datos independientes.
func mockEntities() []Entity {
	// 3 bodies distintos asignados round-robin (i%3) — garantiza variedad
	// visual en el tab "Issue" del drawer sin inventar copy por cada card.
	// Cada uno copia el estilo de issues reales de che: Contexto / Solución
	// propuesta / Aceptación.
	bodies := [3]string{
		`## Contexto
Queremos que el dashboard muestre auto-refresh del board cada N segundos sin reload manual.

## Solución propuesta
HTMX polling con hx-trigger="every 15s" sobre un wrapper .dash-board que se swappea con /board.

## Aceptación
- [ ] Board se refresca sin intervención
- [ ] Drawer no se cierra durante refresh
- [ ] Chip de status indica siguiente poll`,
		`## Contexto
El flow iterate aplica comments/reviews de un PR pero no persiste el verdict previo en el loop.

## Solución propuesta
Guardar el último ` + "`validated:*`" + ` en metadata del loop y exponerlo en el prompt de iterate para que Claude lo considere.

## Aceptación
- [ ] iterate lee el último verdict antes de loopear
- [ ] validate sigue setteando el label sin regresión
- [ ] tests e2e cubren el handoff`,
		`## Contexto
Auto-loop ejecuta explore → validate → execute encadenados, pero si un paso intermedio falla el lock queda colgado.

## Solución propuesta
Wrap de cada paso en defer cleanup que quite el label che:locked sin importar el exit path.

## Aceptación
- [ ] lock se libera en panic
- [ ] lock se libera en error de subprocess
- [ ] tests unitarios del cleanup`,
	}
	e := []Entity{
		// idea — issues idle (2 entidades para que la columna no quede sola).
		{
			Kind: KindIssue, IssueNumber: 61, IssueTitle: "pagination en /api/runs",
			Type: "feature", Size: "m", Status: "idea",
		},
		{
			Kind: KindIssue, IssueNumber: 58, IssueTitle: "timeout configurable por flow",
			Type: "mejora", Size: "s", Status: "idea",
		},

		// planning — issue corriendo `che explore` (transient, hot).
		{
			Kind: KindIssue, IssueNumber: 7, IssueTitle: "che dash web local",
			Type: "feature", Size: "l", Status: "planning",
			RunningFlow: "explore", RunIter: 1, RunMax: 3,
		},

		// plan — explore terminó OK; una con plan-validated:approve y otra
		// sin validar para mostrar las 2 variantes visuales.
		{
			Kind: KindIssue, IssueNumber: 38, IssueTitle: "rollback en che idea",
			Type: "feature", Size: "m", Status: "plan",
			PlanVerdict: "approve",
		},
		{
			Kind: KindIssue, IssueNumber: 50, IssueTitle: "retry en gh api",
			Type: "fix", Size: "xs", Status: "plan",
		},

		// executing — issue+PR fusionados, locked, execute corriendo (hot).
		{
			Kind: KindFused, IssueNumber: 33, IssueTitle: "refactor logger unificado",
			PRNumber: 48, PRTitle: "refactor logger unificado",
			Type: "mejora", Size: "l", Status: "executing",
			Locked:      true,
			RunningFlow: "execute", RunIter: 2, RunMax: 5,
			Branch: "feat/logger-unif", SHA: "3c12aa8",
			LastAction: "execute → iterate #2 (hace 18s)",
			NextAction: "execute corriendo",
			LoopSpec:   "execute · run 2/5",
		},

		// executed — execute terminó OK, esperando validate.
		{
			Kind: KindFused, IssueNumber: 29, IssueTitle: "timeout config por flow",
			PRNumber: 44, PRTitle: "timeout config por flow",
			Type: "mejora", Size: "s", Status: "executed",
			ChecksOK:   5,
			ChecksFail: 2,
			Branch:     "feat/timeout-cfg", SHA: "ff4e9b2",
			LastAction: "execute terminó OK (hace 2m)",
			NextAction: "esperando validate",
		},

		// validating — el caso "selected" del mockup, transient (hot), con
		// LogRuns populados para mostrar el drawer rico.
		{
			Kind: KindFused, IssueNumber: 42, IssueTitle: "fusion entidad issue+PR en dash",
			PRNumber: 55, PRTitle: "fusion entidad issue+PR en dash",
			Type: "feature", Size: "l", Status: "validating",
			ChecksOK:      8,
			ChecksPending: 1,
			RunningFlow:   "validate", RunIter: 3, RunMax: 10,
			Branch: "feat/dash-fusion", SHA: "a8f3c21",
			LastAction: "iterate #3 → validate (hace 42s)",
			NextAction: "validate en ~18s",
			LoopSpec:   "iterate ↔ validate · run 3/10 · stop at approve",
			LogRuns: []LogRun{
				{
					Label: "── run 2 · validate · verdict: changes-requested · hace 1m ──",
					Entries: []LogEntry{
						{Time: "12:03:50", Class: "info", Text: "validate started"},
						{Time: "12:03:58", Text: "feedback: \"log drawer global, no per-tab\""},
						{Time: "12:04:04", Class: "warn", Text: "verdict: changes-requested"},
					},
				},
				{
					Label: "── run 3 · iterate · running ──",
					Entries: []LogEntry{
						{Time: "12:04:31", Class: "info", Text: "iterate started"},
						{Time: "12:04:31", Class: "tool", Text: "[tool] Read internal/dash/server.go"},
						{Time: "12:04:32", Class: "tool", Text: "[tool] Grep \"dash\" cmd/"},
						{Time: "12:04:35", Text: "applying feedback from validate run 2"},
						{Time: "12:04:38", Class: "tool", Text: "[edit] internal/dash/templates/drawer.html.tmpl (+18 -4)"},
						{Time: "12:04:41", Class: "ok", Text: "go build ok"},
						{Time: "12:04:42", Class: "ok", Text: "go test ./internal/dash/... ok"},
						{Time: "12:04:43", Class: "warn", Text: "1 lint warning: unused import"},
						{Time: "12:04:45", Text: "pushing to origin feat/dash-fusion"},
					},
				},
			},
		},

		// validated — 2 entidades cubren los 3 verdicts en uso (approve y
		// changes-requested + iterate corriendo encima).
		{
			Kind: KindFused, IssueNumber: 22, IssueTitle: "listar ideas sin clasificar en TUI",
			PRNumber: 40, PRTitle: "listar ideas sin clasificar en TUI",
			Type: "feature", Size: "s", Status: "validated",
			PRVerdict: "approve",
			ChecksOK:  9,
			Branch:    "feat/list-unclassified", SHA: "77e210d",
			LastAction: "validate run 2 → approve (hace 12m)",
			NextAction: "esperando close",
			LoopSpec:   "iterate ↔ validate · run 2/5 · stop at approve",
		},
		{
			Kind: KindFused, IssueNumber: 18, IssueTitle: "drawer pestaña Issue muestra body",
			PRNumber: 36, PRTitle: "drawer pestaña Issue muestra body",
			Type: "feature", Size: "m", Status: "validated",
			PRVerdict:     "changes-requested",
			ChecksOK:      6,
			ChecksPending: 1,
			RunningFlow:   "iterate", RunIter: 4, RunMax: 8,
			Branch: "feat/drawer-issue-tab", SHA: "1d0e4b9",
			LastAction: "validate run 4 → changes-requested (hace 3m)",
			NextAction: "iterate aplicando feedback",
			LoopSpec:   "iterate ↔ validate · run 4/8 · stop at approve",
		},

		// closing — close en curso (transient, hot).
		{
			Kind: KindFused, IssueNumber: 12, IssueTitle: "che close gates con verdict",
			PRNumber: 28, PRTitle: "che close gates con verdict",
			Type: "mejora", Size: "s", Status: "closing",
			RunningFlow: "close", RunIter: 1, RunMax: 1,
			Branch: "feat/close-gates", SHA: "9b2e7c1",
			LastAction: "close iniciado (hace 12s)",
			NextAction: "mergeando PR",
		},

		// closed — terminal, sin flow corriendo.
		{
			Kind: KindFused, IssueNumber: 5, IssueTitle: "logger unificado v0",
			PRNumber: 9, PRTitle: "logger unificado v0",
			Type: "mejora", Size: "m", Status: "closed",
			PRVerdict: "approve",
			ChecksOK:  10,
			Branch:    "feat/logger-v0", SHA: "0a1b2c3",
			LastAction: "close terminó OK (hace 2h)",
		},
	}
	// Asignación round-robin de los 3 bodies. Mantener fuera del literal
	// de arriba evita repetir `IssueBody: bodies[N]` en cada entry y que
	// alguien al agregar otra se olvide de setearlo.
	for i := range e {
		e[i].IssueBody = bodies[i%len(bodies)]
	}
	return e
}
