# che-cli

CLI en Go para estandarizar el workflow de trabajo con agentes de IA sobre issues de GitHub. El objetivo es medir y reducir el miss rate (alucinación, incompletitud, off-target) forzando que cada unidad de trabajo pase por el mismo embudo.

## Diseño

El diseño completo (flujos, diagramas, walkthrough punta a punta, observabilidad) está en [`design.html`](./design.html). Abrilo en el browser antes de escribir código.

## Principios

- **Stack**: Go + `gh` CLI + CLIs de agentes (`claude`, `codex`, `gemini`) invocados como subprocess.
- **Sin API keys**: se usa la suscripción a cada agente via su CLI, no la API directa.
- **GitHub como único estado**: sin SQLite, sin archivos de sesión. Todo vive en issues, PRs y comments.
- **Stateless por agente**: cada call a un CLI recibe el contexto que necesita desde GitHub + codebase. No hay memoria de sesión.
- **Cada agente comenta en GitHub**: issue para explore, PR para execute.
- **Comments con header estructurado**: `<!-- claude-cli: flow=X iter=N agent=Y role=Z -->` para tracking de iteraciones sin estado externo.
- **Complejidad la asigna la IA**: type (feature/fix/mejora/ux) y size (XS/S/M/L/XL) los decide Claude Sonnet al crear el issue.
- **Veredicto final 100% humano**: al cerrar una idea, el usuario puntúa del 1 al 10 en tres ejes (completitud, fidelidad, alineación) + nota libre.

## Flujos

| # | Comando | IA | Qué hace |
|---|---------|----|----|
| 01 | `che idea` | sí (Sonnet) | Anota idea, decide split, clasifica, crea issue(s) |
| 02 | `che explore` | sí (Opus/Codex/Gemini + 2-3 validadores) | Convierte issue en plan, con iteración inline |
| 03 | `che execute` | sí (Opus/Codex/Gemini + 2-3 validadores) | Implementa en worktree, abre PR, valida contra diff, CI como gate |
| 04 | `che close` | no | Scoring humano + merge del PR + cleanup |
| 05 | `che eliminar` | no | Descarta idea en cualquier estado |

## Observabilidad (v1)

1. Feedback en vivo en terminal (timestamp + agente + estado).
2. Logs estructurados JSONL en `.che-cli/logs/<issue-id>/<flow>-<ts>.jsonl` + blobs de prompts/outputs indexados por hash.
3. Summary al final de cada flow (tiempo, calls, tokens, costo).

## Pre-conditions globales

- Estar parado en un repo con remote de GitHub
- `gh auth status` verde
- CLI `claude` disponible en el PATH
- Los CLIs de `codex` y `gemini` se chequean just-in-time cuando se los selecciona

## Siguientes pasos

1. Scaffolding: `go mod init`, estructura de carpetas, cobra.
2. Comando `che idea` end-to-end como primer hito (sin validadores, sin worktrees — es el más simple).
3. Después `che explore`, `che execute`, `che close`, `che eliminar` en ese orden.
4. Observabilidad capa 1 (terminal) primero, capa 2 (JSONL + blobs) después.
