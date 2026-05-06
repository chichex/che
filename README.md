# che-cli

CLI en Go para estandarizar el workflow de trabajo con agentes de IA (Claude / Codex / Gemini) sobre issues y PRs de GitHub. El objetivo es medir y reducir el miss rate (alucinación, incompletitud, off-target) forzando que cada unidad de trabajo pase por el mismo embudo y deje rastro auditable en GitHub.

## Diseño

El diseño completo (flujos, diagramas, walkthrough punta a punta, observabilidad) está en [`design.html`](./design.html). Abrilo en el browser antes de escribir código.

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

La TUI y los subcomandos comparten el mismo motor; los subcomandos siguen existiendo para automatización.

## Principios

- **Stack**: Go + `gh` CLI invocado como subprocess.
- **Sin API keys**: se usa la suscripción a cada agente vía su CLI, no la API directa.
- **GitHub como único estado**: sin SQLite, sin archivos de sesión. Todo vive en issues, PRs, labels y comments.
- **Máquina de labels `che:*`**: 9 estados (`idea → planning → plan → executing → executed → validating → validated → closing → closed`) con `che:locked` ortogonal como mutex por ref.

## Subcomandos disponibles

| Comando | Qué hace |
|---------|----------|
| `che dash` | Dashboard web local (Kanban por status, drawer con metadata + logs, auto-loop opcional). |
| `che doctor` | Chequea entorno (gh auth, CLIs de agentes en PATH, etc.). |
| `che upgrade` | Actualiza che a la última versión publicada. |

La TUI interactiva (`che` sin args) permite además gestionar locks colgados (`che:locked`).

## Pre-conditions globales

- Estar parado en un repo con remote de GitHub.
- `gh auth status` verde.
- `che doctor` verifica todo lo de arriba.

## Observabilidad

1. Labels `che:*`, `plan-validated:*`, `validated:*` reflejan el estado de la máquina sobre el issue / PR.
2. `che dash` agrega Kanban + drawer + auto-loop como vista en vivo del workflow.

## Desarrollo

```sh
make build      # compila a ./bin/che con la versión derivada de git describe
make install    # instala en /usr/local/bin (codesign en macOS)
make test       # go test ./...
make release    # goreleaser release --clean
```

El árbol de paquetes:

- `cmd/` — entrypoints cobra de cada subcomando.
- `internal/labels/` — máquina de estados `che:*` y mutex `che:locked`.
- `internal/dash/` — server + handlers + auto-loop del dashboard local.
- `internal/tui/` — TUI bubbletea (entry point por defecto).
- `internal/output/` — logger unificado (stdout=payload, stderr=logs).
- `e2e/` — harness e2e con fakes polimórficos.
