# Plan: Conectar rail y pipeline detail de che dash a datos reales

Refs #119 · https://github.com/chichex/che/issues/119

## Contexto

El subcomando `che dash` (#116) sirve un HTML standalone con todos los datos mockeados en JS. Esta entrega expone dos endpoints HTTP read-only y wirea el frontend del dash a datos reales leídos desde `~/.che/pipelines/`. Tabla de runs y botones de acción quedan visualmente deshabilitados con copys literales hasta que llegue spec 2 (runs).

## Objetivo

Reemplazar los mocks JS de pipelines del prototipo por fetch real a `GET /api/pipelines` y `GET /api/pipelines/:slug`, leyendo los YAMLs del usuario sin escribir nada ni tocar el runner. El PR construye sobre la branch `feat/dash-command` (#116), no sobre main.

## Approach

Agregar `internal/dash/pipelines.go` con dos handlers parametrizados por `pipelinesDir`, reusando `wizard.Load(path)` del paquete `internal/wizard` (sin tocar wizard). Reemplazar el HTML embebido actual (1.7MB inline, `che dash.standalone.html`) por la versión "src" del handoff (79KB, React/Tailwind/Babel vía CDN — sigue siendo single file, sin build step). Patch puntual al JS: dos `useEffect` (lista + detail), copys de empty states, props `disabled` en los tres botones (Run/Cancel/Resume).

## Pasos

- [ ] Crear `internal/dash/pipelines.go` con `handleListPipelines(dir string) http.HandlerFunc` y `handleGetPipeline(dir string) http.HandlerFunc`.
- [ ] Walker en el handler de listing: `filepath.Glob("$dir/*.yaml")` (o `os.ReadDir`), `wizard.Load(path)` por archivo, parse errors logueados a stderr con prefijo `[dash]` y skippeados (no rompen el endpoint).
- [ ] Detección ready/draft: `Pipeline.Status == nil` → ready; no-nil → draft.
- [ ] Slug = filename sin extensión.
- [ ] `handleGetPipeline` parsea el slug del path (`strings.TrimPrefix(r.URL.Path, "/api/pipelines/")`), 404 + JSON `{"error":"pipeline not found"}` si no existe.
- [ ] Registrar rutas en `internal/dash/dash.go`: `mux.HandleFunc("/api/pipelines", ...)` (match exacto) + `mux.HandleFunc("/api/pipelines/", ...)` (prefijo, detail).
- [ ] `Serve()` resuelve `pipelinesDir` via `os.UserHomeDir()` + `filepath.Join(".che", "pipelines")`. Si falla, `pipelinesDir = ""` y los handlers devuelven `[]` / 404.
- [ ] Reemplazar `internal/dash/assets/dash.html` con el archivo `che dash standalone src.html` del bundle de Claude Design (79KB, CDN-based).
- [ ] Patch JS sobre el src:
  - `const PIPELINES_INIT = []` (vaciar el array de mocks)
  - `const RUNS_INIT = {}` (vaciar el mock de runs para que la tabla muestre empty state)
  - useEffect en el componente App: `fetch('/api/pipelines').then(r=>r.json()).then(setPipelines)`
  - useEffect cuando cambia el slug seleccionado: `fetch('/api/pipelines/' + slug).then(...).then(setPipelineDetail)` y merge de `steps` al state
  - Empty state literales:
    - Rail vacío: `no pipelines yet — corré che y entrá a Create pipeline`
    - Tabla de runs: `sin runs registrados — corré el pipeline desde la TUI`
    - Draft incompleto (sin steps): `draft incompleto — terminar en la TUI`
  - Botones Run / Cancel / Resume: prop `disabled` + `title="v1 read-only — usar TUI para lanzar"`. Estilos visualmente apagados (`disabled:opacity-50 disabled:cursor-not-allowed`).
  - Selección inicial al cargar: primer ready alfabético (o ninguno si solo hay drafts) — mantener la lógica del prototipo, adaptada a state asíncrono.
- [ ] Tests `internal/dash/pipelines_test.go`: empty dir → `[]`; un ready → status `ready`; un draft → status `draft`; mixed → ambos; YAML corrupto → excluido + log; slug missing en detail → 404; slug correcto → 200 con steps; validator opcional → omitido si nil.
- [ ] Smoke test manual documentado en el PR description: `go build && ./che dash --no-open --port 17878 && curl http://127.0.0.1:17878/api/pipelines`.

## Archivos afectados

- `internal/dash/pipelines.go` — crear — handlers + helpers de listing/detail
- `internal/dash/pipelines_test.go` — crear — coverage de los 7 casos listados
- `internal/dash/dash.go` — modificar — registrar 2 rutas + resolver `pipelinesDir` en `Serve()`
- `internal/dash/assets/dash.html` — reemplazar (src en vez de standalone) + patches JS puntuales

## Riesgos

- Patch al React vía Babel-in-browser: si el flujo de hooks se rompe, el dash queda en blanco. Mitigación: cambios localizados a 2 `useEffect` en el componente App; smoke visual antes de marcar done.
- Pérdida de modo offline: el dash pasa a depender de CDN (unpkg + fonts.google) para cargar React/Tailwind/Babel. Trade-off aceptable (dev tool local, dev tiene internet); recuperable en una iteración futura si hace falta.
- `wizard.Load` puede tener side effects no documentados. Mitigación: lo invocamos dentro de un loop simple con log de errores; no bloqueamos el endpoint ante un YAML particular.
- Bundle del binario achica 1.7MB → 79KB (mejora secundaria, no riesgo).
- El nombre `feat/dash-command` (#116) debe seguir existiendo en remote cuando se cree este PR. Si #116 fue cerrado, el PR de plan apuntaría a una branch huérfana.

## Out of scope

- Runs (spec 2)
- SSE per-run (spec 3) y SSE global (spec 4)
- Lanzar / cancelar / resume desde el dash (write API, eventual spec futuro)
- Hot-reload si el usuario edita un YAML mientras el dash está abierto
- Inline offline de React / Tailwind / Babel (recuperable si más adelante hace falta)
- Cambios al package `internal/wizard` (mantenemos blast radius bajo; usamos solo APIs públicas)

## Asunciones tecnicas validadas

1. Stack: Go puro, sin deps nuevas. Reuse de `encoding/json` (stdlib) y `internal/wizard` (parser YAML existente con `gopkg.in/yaml.v3`).
2. Ubicación: nuevo archivo `internal/dash/pipelines.go` + tests `internal/dash/pipelines_test.go`. Sin sub-paquete nuevo.
3. Reuso de `wizard.Load(path)` para cada archivo individual. NO se modifica el package wizard.
4. Listing: `filepath.Glob("$dir/*.yaml")` o `os.ReadDir`. Solo extension `.yaml` (no `.yml`).
5. Slug: filename sin extensión. Igual que la TUI.
6. ready vs draft: `Pipeline.Status == nil` → ready; no-nil → draft. Espeja la convención del wizard.
7. JSON shape de `GET /api/pipelines`: array de `{slug, name, description, status}`. Sin `last_run` (spec 2 lo agrega).
8. JSON shape de `GET /api/pipelines/:slug`: `{slug, name, description, status, steps: [{name, cli, kind, content, input, validator?}]}`. `validator` omitido (no `null`) si falta.
9. Manejo de errores HTTP: 404 + JSON `{"error":"pipeline not found"}` para slug missing; 500 para errores sistémicos raros; 200 con `[]` para empty dir.
10. Routing: dos handlers separados. `mux.HandleFunc("/api/pipelines", listHandler)` (match exacto) + `mux.HandleFunc("/api/pipelines/", detailHandler)` con slug extraído por `strings.TrimPrefix`.
11. Inyección de dependencias: handlers se construyen con factory `func(dir string) http.HandlerFunc`. Tests pasan `t.TempDir()`. Producción resuelve via `os.UserHomeDir()`.
12. Parse errors: pipeline corrupto excluido de la lista, log a stderr con `log.Printf` prefijo `[dash]`.
13. Reemplazo del HTML embebido: pasar de `che dash.standalone.html` (1.7MB, todo inline) a `che dash standalone src.html` (79KB, CDN-based para React/Tailwind/Babel/fonts). Trade-off offline→online aceptable.
14. Patch JS: dos hooks en el componente raíz. (a) `useState([])` + `useEffect` que hace fetch `/api/pipelines` y `setPipelines`. (b) Cuando cambia el slug seleccionado, fetch `/api/pipelines/:slug` y merge de `steps` al state local.
15. Empty state copies: literal exacto del spec, aplicado via Edit tool sobre el src.
16. Botones Run/Cancel/Resume: prop `disabled` + `title="v1 read-only — usar TUI para lanzar"`. Estilos disabled ya cubiertos por Tailwind utility classes.
17. Selección inicial post-fetch: lógica existente del prototipo (primer ready) se preserva o se replica si el código del src la hace condicional al `PIPELINES_INIT`.
18. Tests: `httptest.NewRecorder` + `t.TempDir()` para fixture isolation. No es necesario monkeypatch `os.UserHomeDir`.
19. Smoke test manual incluido en el PR description: `go build && ./che dash --no-open --port 17878` + `curl` calls.

---

_Plan generado por `/hs-plan` a partir de #119._
