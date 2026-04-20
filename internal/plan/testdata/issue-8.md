## Plan consolidado (post-exploración)

**Resumen:** Agregar signal handling explícito (SIGINT/SIGTERM) en `che execute` usando `signal.NotifyContext` para que Ctrl+C o `kill` disparen cleanup determinista síncrono del worktree, branch local y label `executing → plan`, incluyendo cancelación del wait async de validadores y cableado de la TUI al mismo context.

**Goal:** Cuando el usuario cancela `che execute` con Ctrl+C o el proceso recibe SIGTERM, el binario termina con exit code 130 después de revertir localmente todo el estado (worktree removido, branch local borrada, label transicionada de `executing` a `plan`) y sin dejar subprocesos zombie, de forma que el próximo `che execute` sobre el mismo issue arranca limpio.

### Criterios de aceptación
- [ ] Ctrl+C durante `agent exec` en CLI deja worktree removido y label en `status:plan`
- [ ] SIGTERM externo (`kill <pid>`) respeta el cleanup local completo antes de salir
- [ ] Exit code 130 al cancelar con SIGINT; distinto de exit code normal de error
- [ ] TUI: Ctrl+C cierra la UI y ejecuta el mismo cleanup que CLI (no solo mata la UI)
- [ ] Test e2e que simula SIGINT durante el stream del agente y verifica por estado final (worktree ausente, label en plan, sin procesos huérfanos) usando un fake agent con sentinel `READY\n`
- [ ] Si la señal llega durante el wait async de validadores (timeout 10min), ese wait se cancela y el cleanup local corre igual
- [ ] No quedan procesos zombie del agente (claude/codex) tras la cancelación
- [ ] Si la señal llega post-`gh pr create` exitoso, el cleanup local corre pero la branch remota y el PR draft quedan intactos (best-effort, loggeado claro)

### Approach
Path recomendado del plan original: reemplazar `context.Background()` en `Run` por `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` y propagarlo a todos los `exec.CommandContext` (agente, git, gh) y al goroutine/canal que espera validadores async. Los subprocesos del agente se invocan con `SysProcAttr.Setpgid=true` para matar el process group entero, con timeout + SIGKILL de escape. Cleanup centralizado en defers idempotentes sensibles a `ctx.Err()`, bloqueando el exit hasta completar (síncrono). La TUI usa `tea.WithoutSignalHandler` y comparte el mismo context cancelable para coordinar shutdown de UI después del cleanup — decisión técnica tomada por el ejecutor, no se re-consulta al humano. Rollback remoto (branch pusheada + PR draft) es best-effort: no se borra ni se cierra automáticamente, se loggea y se deja para retry manual.

### Pasos
1. Auditar en `internal/flow/execute/execute.go` todas las invocaciones de subprocesos (agente, git, gh) y migrar de `exec.Command` a `exec.CommandContext`, seteando `SysProcAttr.Setpgid=true` donde corresponda para habilitar kill del process group
2. Reemplazar `context.Background()` en `Run` (L201-372) por `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` y propagar ese context a agente, git, gh, y al wait async de validadores
3. Hacer los defers de rollback (L272-282) idempotentes y sensibles a `ctx.Err()`: cada paso verifica si ya corrió y puede ejecutarse dos veces sin romper; definir orden fijo (label `executing → plan` primero, luego worktree remove, luego branch local)
4. Usar `git worktree remove --force` en el cleanup por señal para evitar fallo con cambios uncommitted del agente; loggear si se forzó
5. Agregar helper que al cancelarse el context mate el process group del agente con SIGTERM, espere N segundos (timeout corto), y fuerce SIGKILL si no terminaron
6. Cancelar el goroutine/canal del wait async de validadores cuando `ctx.Done()` dispara, para que no sigan corriendo en background tras el cleanup local
7. En `cmd/execute.go`, retornar exit code 130 cuando `ctx.Err() == context.Canceled` por señal (distinguir de error normal)
8. En `internal/tui/model.go`, configurar bubbletea con `tea.WithoutSignalHandler` y compartir el mismo context cancelable de `Run`; emitir `tea.Msg` cuando el context se cancele para cerrar la UI después de que el cleanup termine
9. Post-`gh pr create`: si la señal llega cuando la branch remota ya existe y el PR draft está creado, saltar el intento de rollback remoto, loggear con mensaje claro al usuario indicando que la branch remota y el PR quedan para retry manual, y seguir con cleanup local (label + worktree + branch local)
10. Agregar test e2e que arranca `che execute` con un fake agent que emite `READY\n` en stdout antes de bloquear, el test espera ese sentinel, manda SIGINT al proceso, y verifica por estado final: worktree ausente, label en `plan`, exit code 130, sin procesos hijos vivos
11. Agregar test e2e para SIGTERM externo (`kill <pid>`) con mismos asserts
12. Agregar test e2e que inyecta señal durante el wait async de validadores y verifica que el cleanup local corre igual y el wait se cancela

### Riesgos a mitigar
- **Cancelar el context no termina los subprocesos del agente (claude/codex) y quedan zombies consumiendo tokens/CPU mientras el cleanup ya corrió** (likelihood=high, impact=high) — Usar `exec.CommandContext` + `SysProcAttr.Setpgid=true` para mandar señal al process group entero; esperar con timeout corto y forzar SIGKILL si no terminan en N segundos
- **Segundo Ctrl+C durante el cleanup deja estado a medias (worktree borrado, label sin revertir, o viceversa)** (likelihood=medium, impact=medium) — Cleanup idempotente con orden fijo (label primero → worktree → branch local); en segunda señal, skipear confirmaciones y forzar exit; los pasos ya ejecutados son no-op al re-correr
- **Conflicto entre el handler default de Ctrl+C de bubbletea y `signal.NotifyContext` causa que la TUI cierre antes del cleanup o que corra dos veces** (likelihood=medium, impact=medium) — `tea.WithoutSignalHandler` + context compartido; la TUI recibe `tea.Msg` cuando el context se cancela y solo ahí cierra la UI, después de que el cleanup del flow principal haya corrido
- **`git worktree remove` falla por cambios uncommitted del agente y deja worktree huérfano pese al handler de señales** (likelihood=medium, impact=medium) — Usar `git worktree remove --force` en el path de cleanup por señal; loggear que se forzó para debugging
- **Test e2e de SIGINT flaky en CI por timing (la señal llega antes de que el agente esté corriendo, o después de que ya terminó)** (likelihood=medium, impact=low) — Fake agent emite `READY\n` al stdout; el test espera ese sentinel antes de mandar la señal; asserts por estado final (worktree ausente + label en plan + exit 130), no por logs intermedios
- **SIGKILL, `kill -9`, OOM killer o panic del kernel no son atrapables y dejan el estado colgado pese al handler** (likelihood=low, impact=medium) — Aceptado para esta iteración; cubrir en follow-up con state file crash-safe + recovery en el próximo `che execute` (ver out_of_scope)
- **Race entre cancelación del wait async de validadores y el propio cleanup: el goroutine del wait sigue vivo después de que `Run` retornó** (likelihood=low, impact=medium) — El goroutine del wait recibe el mismo context; `Run` bloquea hasta que ese goroutine confirme exit vía done channel antes de retornar

### Fuera de alcance
- Rollback automático de branch remota pusheada tras la señal (best-effort: se deja para retry manual)
- Cerrar/borrar PR draft automáticamente tras la señal (best-effort: se deja para retry manual)
- State file crash-safe (`.che-state.json`) + recovery automático en el próximo `che execute` para cubrir SIGKILL/OOM/panic — queda como follow-up complementario, no contradice este approach
- `che execute --cleanup <issue>` subcomando manual de cleanup
- Persistir intención de rollback en disco para sobrevivir a doble Ctrl+C durante el propio cleanup (se confía en idempotencia)

---

## Idea original (de `che idea`)

## Idea
Agregar signal handling explícito (SIGINT/SIGTERM) en `che execute` para garantizar cleanup determinista del worktree, branch local y transición de label cuando el usuario cancela con Ctrl+C o el proceso es matado.

## Contexto detectado
- Archivos/módulos relevantes: `internal/flow/execute/execute.go` (`Run` en L201-372, defer de rollback en L272-282), `cmd/execute.go`, `internal/tui/model.go` (pantallas execute)
- Área afectada: ejecución local de `che execute`
- Deuda declarada en PR #7 (v0.0.20) y issue #6 ("fuera de alcance").

## Problema
Hoy el cleanup depende solo de `defer`. Si el usuario hace Ctrl+C durante el agente, o el proceso recibe SIGTERM (shell cerrado, `kill <pid>`, máquina apagada), Go mata el proceso sin correr defers → **worktree huérfano en `.worktrees/issue-N` + label stuck en `status:executing` para siempre**. El próximo `che execute` sobre ese issue falla con "already executing" y el usuario tiene que limpiar a mano.

Es la falla más probable en uso real del feature.

## Propuesta
- Capturar `os.Interrupt` y `syscall.SIGTERM` en `Run` vía `signal.NotifyContext` o equivalente.
- Al recibir señal: cancelar el `context.Context` del agente (hoy es `context.Background()` con timeout), esperar con timeout corto a que los subprocesos terminen, y correr el cleanup completo (remove worktree, borrar branch local, revertir label `executing → plan`).
- Retornar con exit code 130 (convención shell para SIGINT).
- En la TUI: cablear el Ctrl+C de bubbletea al mismo cancelable context para que cierre la TUI + limpie consistentemente.

## Criterios de éxito iniciales
- [ ] Ctrl+C durante `agent exec` deja worktree limpio y label en `status:plan`
- [ ] SIGTERM externo (`kill <pid>`) respeta el cleanup
- [ ] Test e2e que simula SIGINT durante stream del agente y verifica rollback
- [ ] Exit code 130 al cancelar con Ctrl+C
- [ ] TUI: Ctrl+C cierra la UI + ejecuta cleanup (hoy solo mata la UI)

## Notas / warnings
- El flow async de validadores (wait con timeout 10min) también debe cancelarse con la señal.
- Cuidar el caso "señal durante `gh pr create` post-push": la branch remota ya existe, decidir si se borra también o se deja para retry manual.

## Clasificación
- Type: feature (robustez operacional)
- Size: M — requiere cambios en `Run`, `cmd/execute.go`, TUI y tests e2e nuevos.

