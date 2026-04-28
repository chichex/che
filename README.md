# che-cli

CLI en Go para estandarizar el workflow de trabajo con agentes de IA (Claude / Codex / Gemini) sobre issues y PRs de GitHub. El objetivo es medir y reducir el miss rate (alucinación, incompletitud, off-target) forzando que cada unidad de trabajo pase por el mismo embudo y deje rastro auditable en GitHub.

## Diseño

El diseño completo (flujos, diagramas, walkthrough punta a punta, observabilidad) está en [`design.html`](./design.html). Abrilo en el browser antes de escribir código.

## Instalación

```sh
# macOS (recomendado, vía homebrew tap)
brew install chichex/tap/che

# Cualquier OS — script de install que baja el binario del último release
curl -sSL https://raw.githubusercontent.com/chichex/che/main/install.sh | sh

# Desde fuente
make install
```

`che upgrade` actualiza a la última versión publicada. `che doctor` chequea que el entorno esté listo.

## Uso

```sh
che               # abre la TUI interactiva (entry point por defecto)
che <subcomando>  # invocación directa, útil para scripting / CI / tests
```

La TUI y los subcomandos comparten el mismo motor; los subcomandos siguen existiendo para automatización.

## Principios

- **Stack**: Go + `gh` CLI + CLIs de agentes (`claude`, `codex`, `gemini`) invocados como subprocess.
- **Sin API keys**: se usa la suscripción a cada agente vía su CLI, no la API directa.
- **GitHub como único estado**: sin SQLite, sin archivos de sesión. Todo vive en issues, PRs, labels y comments.
- **Stateless por agente**: cada call a un CLI recibe el contexto que necesita desde GitHub + codebase. No hay memoria de sesión.
- **Comments con header estructurado**: `<!-- che-cli: flow=X iter=N agent=Y role=Z -->` para tracking de iteraciones sin estado externo.
- **Máquina de labels `che:*`**: 9 estados (`idea → planning → plan → executing → executed → validating → validated → closing → closed`) con `che:locked` ortogonal como mutex por ref.
- **Validación explícita**: `explore` y `execute` no validan auto; `validate` lockea, corre validadores y transiciona. `iterate` aplica los findings.
- **Veredicto final 100% humano**: `che close` deja el merge y la nota final al usuario; che warnea pero no rechaza.

## Flujos

| Comando | IA | Qué hace |
|---------|----|----------|
| `che idea [texto]` | Sonnet | Anota una idea, decide split, clasifica, crea issue(s) con label `che:idea`. |
| `che explore <issue>` | Opus / Codex / Gemini | Convierte un issue en plan consolidado (`che:plan`), iterando inline. |
| `che execute <issue>` | Opus / Codex / Gemini | Implementa en worktree aislado y abre un PR draft contra `main`. Acepta `che:idea` o `che:plan`. |
| `che validate <ref>` | validadores en paralelo | Corre validadores (opus/codex/gemini) sobre un plan (issue) o PR; postea findings y aplica `plan-validated:*` / `validated:*`. |
| `che iterate <ref>` | Opus | Aplica los findings de `che validate` sobre el plan o el PR. |
| `che close <pr>` | — | Saca de draft, mergea, cierra el issue asociado. El veredicto y el merge los decide el humano. |
| `che dash` | — | Dashboard web local (Kanban por status, drawer con metadata + logs, auto-loop opcional). |
| `che unlock <ref>` | — | Escape hatch: quita `che:locked` si un flow quedó colgado. |
| `che migrate-labels` | — | Migra repos viejos del modelo `status:*` al actual `che:*`. |
| `che doctor` | — | Chequea entorno (gh auth, CLIs de agentes en PATH, etc.). |
| `che upgrade` | — | Actualiza che a la última versión publicada. |

## Pre-conditions globales

- Estar parado en un repo con remote de GitHub.
- `gh auth status` verde.
- CLI `claude` disponible en el PATH.
- Los CLIs de `codex` y `gemini` se chequean just-in-time cuando se los selecciona.
- `che doctor` verifica todo lo de arriba.

## Observabilidad

1. Feedback en vivo en terminal y en la TUI (timestamp + agente + estado).
2. Comments en GitHub con header estructurado por flow / iter / agente / role.
3. Labels `che:*`, `plan-validated:*`, `validated:*` reflejan el estado de la máquina sobre el issue / PR.
4. `che dash` agrega Kanban + drawer + auto-loop como vista en vivo del workflow.

## Desarrollo

```sh
make build      # compila a ./bin/che con la versión derivada de git describe
make install    # instala en /usr/local/bin (codesign en macOS)
make test       # go test ./...
make release    # goreleaser release --clean
```

El árbol de paquetes:

- `cmd/` — entrypoints cobra de cada subcomando.
- `internal/flow/` — flows compartidos (idea, explore, execute, validate, iterate, close).
- `internal/agent/` — abstracción `Agent` / `Validator` / `Run` invocando a los CLIs externos.
- `internal/labels/` — máquina de estados `che:*` y mutex `che:locked`.
- `internal/dash/` — server + handlers + auto-loop del dashboard local.
- `internal/tui/` — TUI bubbletea (entry point por defecto).
- `internal/output/` — logger unificado (stdout=payload, stderr=logs).
- `e2e/` — harness e2e con fakes polimórficos.
