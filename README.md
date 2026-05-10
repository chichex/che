# che-cli

CLI en Go que orquesta pipelines parametrizables sobre múltiples agentes de IA (Claude, Codex, Gemini, opencode). El usuario define los steps de su pipeline en YAML; cada step usa el CLI que elija, opcionalmente cross-validado por otro CLI distinto, en loop hasta que converge. Sin API keys: usa la suscripción del usuario en cada CLI.

## Diseño

Los documentos `docs/*.html` se renderizan via [htmlpreview.github.io](https://htmlpreview.github.io/) (los enlaces de abajo abren la versión renderizada; el path entre paréntesis es la fuente):

- [Visión del producto](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/vision.html) (`docs/vision.html`)
- [Decisiones de arquitectura cerradas](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/design.html) (`docs/design.html`)
- [Flujo "Create / My pipelines"](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/manage-pipelines-flow.html) (`docs/manage-pipelines-flow.html`) — wizard S1–S4 + lister de drafts/ready.
- [Flujo de ejecución de pipelines](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/pipeline-execution-flow.html) (`docs/pipeline-execution-flow.html`) — runner R0–RF, validator loop, manifest atómico, addenda post-H10.

## Instalación

```sh
# macOS (recomendado, vía homebrew tap)
brew install --cask chichex/tap/che

# Cualquier OS — script de install que baja el binario del último release
curl -sSL https://raw.githubusercontent.com/chichex/che/main/install.sh | sh

# Desde fuente
make install
```

`che upgrade` actualiza a la última versión publicada. `che doctor` chequea que el entorno esté listo.

### Versión beta (pre-release)

Las versiones beta se publican con tags `vX.Y.Z-rc.N`. No se garantiza estabilidad de API ni backward compatibility.

```sh
# Primera instalación
brew install --cask chichex/tap/che-beta

# Upgrade a una versión nueva
brew update                       # refresca el tap local
brew upgrade --cask che-beta      # detecta versión nueva y upgradea

# Verificar
che-beta --version
```

`brew install` es no-op si el cask ya está instalado: NO upgradea. Si `brew upgrade` no detecta cambio (el cask deshabilita livecheck — lo regenera goreleaser en cada release), forzá con `brew reinstall --cask che-beta`.

El cask beta instala el comando como `che-beta` (no `che`), así puede convivir con el estable. Para correr el RC invocá `che-beta` directamente — `che` sigue apuntando al binario estable.

## Uso

```sh
che               # abre la TUI interactiva (entry point por defecto)
che <subcomando>  # invocación directa, útil para scripting / CI / tests
```

La TUI tiene 5 entradas:

| # | Entrada | Qué hace |
|---|---------|----------|
| 1 | **My pipelines** | Lista los pipelines en `~/.che/pipelines/`, mezclando ready y drafts, con chip del último run por row. Acciones: <kbd>enter</kbd> reanuda un draft o **ejecuta** un ready (entra al runner R0→R1→R2→R3→R4/RF), <kbd>e</kbd> reedita un ready, <kbd>d</kbd> borra (con confirm), <kbd>y</kbd> abre el YAML en `$EDITOR`, <kbd>r</kbd> abre la sub-screen "Run history" del row. |
| 2 | **Create pipeline** | Wizard que termina en un YAML ready en `~/.che/pipelines/<slug>.yaml`. Persistencia incremental (el archivo es "draft" mientras el wizard no haya cerrado el bloque `status`; se vuelve "ready" al finalizar). Incluye prompt review IA opcional sobre cada step. |
| 3 | **Crear pipeline con IA** | Genera un pipeline a partir de una descripción libre, lo deja en formato draft listo para revisar en el wizard. |
| 4 | **See skills** | Detecta los skills instalados en los 4 CLIs (claude / codex / gemini / opencode) y permite abrir cada `SKILL.md` en VS Code. Solo lectura. |
| 0 | **Exit** | Salir. |

## Anatomía de un pipeline

Un pipeline ready vive en `~/.che/pipelines/<slug>.yaml` y describe N steps. Cada step elige su CLI, su modo (`prompt` libre o `skill` instalado), su input y, opcionalmente, un validator que cross-revisa el output en loop antes de avanzar al siguiente.

```yaml
name: Triage incident
description: toma una métrica anómala y dispara un triage
steps:
  - name: collect-signals
    cli: claude
    kind: prompt
    content: extrae las métricas anómalas del payload
    input: text
  - name: cross-check
    cli: codex
    kind: skill
    content: triage-runbook
    input: previous_output
    validator:
      cli: gemini
      kind: prompt
      content: verifica que el output respete el formato pedido
    max_loops: 3
    on_max_loops: fail
```

Decisiones cerradas — ver [docs/design.html renderizado](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/design.html):

- Subprocess + headless con `stream-json` (no PTY).
- 4 CLIs v1 con sus modos de invocación: claude `-p`, codex `exec`, gemini `-p ... --yolo`, opencode `run`.
- YAML estricto: el parser rechaza type coercion silente.
- Validator opcional, CLI **independiente** del step (cross-review). `on_max_loops`: `fail` (default) / `continue` / `pause`.
- Sin gates humanos, sin "premisa" abstracta, sin SQLite — un único archivo por pipeline.

## Ejecutar un pipeline

Desde **My pipelines**, <kbd>enter</kbd> sobre un row con chip `ready` entra al runner. Diagrama de estados completo en [`docs/pipeline-execution-flow.html`](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/pipeline-execution-flow.html). Resumen:

| Estado | Qué pasa |
|--------|----------|
| **R1 · InputPrompt** | Pide y resuelve eagerly el input del primer step según `input` (`text` / `file` / `url` / `pr` / `issue`). `none` skipea directo a R2. |
| **R2 · Preflight** | Chequea CLI requerido, skill instalado, `gh auth` y disco. Verdict + <kbd>r</kbd> retry. |
| **R3 · Running** | Spawnea cada step con `stream-json`, log pane viviente con sticky-bottom + ring buffer, chaining vía `previous_output`, validator loop honrando `max_loops` + `on_max_loops`. <kbd>tab</kbd> cicla entre steps. |
| **R4 / RF · Done** | Modal final (ok / failed). Teclas: <kbd>y</kbd> copy output, <kbd>l</kbd> abrir log, <kbd>r</kbd> retry pre-cargando el input. |
| **RC · Cancel** | <kbd>ctrl+c</kbd> dispara graceful shutdown (SIGTERM→SIGKILL) y cleanup de handles. |
| **RP · Pause** | Sólo si algún step define `on_max_loops: pause`. Espera decisión humana. |

Cada run persiste un manifest atómico (`.tmp` + rename) bajo `~/.che/runs/<run-id>/`. Al boot, `My pipelines` recovera runs interrumpidos y los marca con chip explícito; un GC limpia runs viejos.

## Subcomandos

| Comando | Qué hace |
|---------|----------|
| `che doctor` | Chequea que los 4 CLIs estén instalados, gh auth, etc. |
| `che upgrade` | Actualiza che a la última versión publicada (`--check` solo informa). |

## Desarrollo

```sh
make build      # compila a ./bin/che con la versión derivada de git describe
make install    # instala en /usr/local/bin (codesign en macOS)
make test       # go test ./...
make release    # goreleaser release --clean
```

Layout:

- `cmd/` — entrypoints cobra (`doctor`, `upgrade`, `root`) + `cmd/fake/` para los e2e.
- `internal/wizard/` — wizard "Create pipeline" + lister "My pipelines" (con chip last-run + sub-screen Run history) + prompt review IA + persist + YAML + IsValid.
- `internal/tui/` — menu principal, "See skills" y "Crear pipeline con IA".
- `internal/runner/` — runner R0–RF (input, preflight, spawn, streaming, validator loop, manifest, cancel, pause, done) + `internal/runner/parser/` (claude / codex / raw stream-json).
- `internal/skills/` — detección de skills en los 4 CLIs.
- `internal/output/` — logger unificado (stdout=payload, stderr=logs).
- `internal/aiprompt/` — generación IA de prompts y de pipelines (entry "Crear pipeline con IA" + prompt review).
- `internal/repoctx/` — resolución del repo del cwd para inputs `pr` / `issue`.
- `internal/clipboard/` — copy del output final desde el modal RF.
- `e2e/` — harness e2e (PTY + fakes polimórficos) y tests por feature (wizard, runner, tui).

Pre-condiciones para correr el binario en local:

- Al menos uno de claude / codex / gemini / opencode instalado y autenticado vía suscripción (no API key).
- `che doctor` verifica el entorno antes de empezar.
