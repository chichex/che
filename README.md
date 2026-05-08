# che-cli

CLI en Go que orquesta pipelines parametrizables sobre múltiples agentes de IA (Claude, Codex, Gemini, opencode). El usuario define los steps de su pipeline en YAML; cada step usa el CLI que elija, opcionalmente cross-validado por otro CLI distinto, en loop hasta que converge. Sin API keys: usa la suscripción del usuario en cada CLI.

## Diseño

Los documentos `docs/*.html` se renderizan via [htmlpreview.github.io](https://htmlpreview.github.io/) (los enlaces de abajo abren la versión renderizada; el path entre paréntesis es la fuente):

- [Visión del producto](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/vision.html) (`docs/vision.html`)
- [Decisiones de arquitectura cerradas](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/design.html) (`docs/design.html`)
- [Plan H1–H10 del flow "Create / My pipelines"](https://htmlpreview.github.io/?https://github.com/chichex/che/blob/main/docs/manage-pipelines-flow.html) (`docs/manage-pipelines-flow.html`) — chips de progreso por story.

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

Para probar features en desarrollo antes del release estable:

```sh
brew install --cask chichex/tap/che-beta
che-beta
```

El cask beta instala el comando `che-beta`, así puede convivir con el estable (`che`).

Las versiones beta se publican con tags `vX.Y.Z-rc.N`. No se garantiza estabilidad de API ni backward compatibility.

## Uso

```sh
che               # abre la TUI interactiva (entry point por defecto)
che <subcomando>  # invocación directa, útil para scripting / CI / tests
```

La TUI tiene 4 entradas:

| # | Entrada | Qué hace |
|---|---------|----------|
| 1 | **My pipelines** | Lista los pipelines en `~/.che/pipelines/`, mezclando ready y drafts. Acciones por row: <kbd>enter</kbd> reanuda un draft o avisa "ejecución no implementada" sobre ready, <kbd>e</kbd> reedita un ready, <kbd>d</kbd> borra (con confirm), <kbd>y</kbd> abre el YAML en `$EDITOR`. |
| 2 | **Create pipeline** | Wizard de 3 pantallas (info → steps → resumen) que termina en un YAML ready en `~/.che/pipelines/<slug>.yaml`. Persistencia incremental (el archivo es "draft" mientras el wizard tenga abierto el bloque `status`; se vuelve "ready" al finalizar). |
| 3 | **See skills** | Detecta los skills instalados en los 4 CLIs (claude / codex / gemini / opencode) y permite abrir cada `SKILL.md` en VS Code. Solo lectura. |
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

- `cmd/` — entrypoints cobra (`doctor`, `upgrade`, `root`).
- `internal/wizard/` — wizard "Create pipeline" + lister "My pipelines" + persist + YAML + IsValid.
- `internal/tui/` — menu principal y pantalla "See skills".
- `internal/skills/` — detección de skills en los 4 CLIs.
- `internal/output/` — logger unificado (stdout=payload, stderr=logs).
- `e2e/` — harness e2e (PTY + fakes polimórficos) y tests.

Pre-condiciones para correr el binario en local:

- Al menos uno de claude / codex / gemini / opencode instalado y autenticado vía suscripción (no API key).
- `che doctor` verifica el entorno antes de empezar.
