# Plan: Sacar indicador 'disconnected' del che dash

Refs #139 - https://github.com/chichex/che/issues/139

## Contexto

El dash (servido por `internal/dash`) muestra un badge "disconnected" en dos lugares cuando el `EventSource` (SSE) entra en estado de error: en el header global (junto al contador de pipelines, con un boton "retry") y arriba a la derecha del panel de output del step seleccionado. La logica los dispara desde el handler `onerror` del SSE, ademas de un `setTimeout(10s)` que reafirma el estado disconnected aunque el browser ya este reintentando.

Hoy ese indicador genera falsos positivos (la conexion en realidad esta sana) y no aporta valor. El `EventSource` ya reconecta solo, asi que sacar el badge no rompe la funcionalidad.

## Objetivo

Eliminar el badge "disconnected" del header global y del panel de step en `internal/dash/assets/dash.html`, junto con todo el estado/timer/handler/UI cuyo unico proposito es servir a ese badge. Mantener el badge "reconnecting…" (informa transicion legitima) y la reconexion silenciosa del SSE.

## Approach

Toda la edicion vive en un unico archivo: `internal/dash/assets/dash.html`. No hay codigo Go ni tests JS asociados — el validator se asegura de que el binario siga compilando y de que el dash cargue sin errores en runtime.

Para cada uno de los dos `EventSource` (global a `/api/events`, per-run a `/api/pipelines/<slug>/runs/<id>/events`):

1. Borrar la rama del JSX que renderiza el badge `disconnected`.
2. En el `onerror`, dejar solo la rama `reconnecting`. Borrar el `else { setX("disconnected") }` y el bloque `setTimeout(10s)` que setea disconnected diferido.
3. Borrar el `useRef` del timer (`disconnectTimerRef` / `globalDisconnTimerRef`) y los `clearTimeout` correspondientes en `onopen` y en el cleanup del `useEffect`. Sin setter, son codigo muerto.
4. Actualizar el comentario inline del `useState` de estado SSE para que liste solo `idle | live | reconnecting` (o `hidden | live | reconnecting`).

Especifico para el header global:

5. Borrar el `useCallback` `retryGlobal` (linea ~1610) — solo se usaba por el boton retry del badge disconnected.
6. Quitar la prop `onRetryGlobal` del componente `TopBar` (firma + uso) y del JSX donde se monta `<TopBar ... onRetryGlobal={retryGlobal} />`.

CSS: la clase `badge-disconn` (linea 137) solo se usaba para este badge → borrarla. `badge-failed`, `badge-reconn`, `dot-failed`, `dot-paused` se reusan en otros badges → no tocarlas.

## Pasos

- [ ] Borrar rama JSX `globalConnState === "disconnected"` (header) en `internal/dash/assets/dash.html` (lineas ~447-454), incluido el boton retry.
- [ ] Borrar rama JSX `sseStatus === "disconnected"` (panel step) en `internal/dash/assets/dash.html` (lineas ~1024-1026).
- [ ] En `openGlobalES.onerror` (~1480-1493): dejar solo el `if (es.readyState === EventSource.CONNECTING) setGlobalConnState("reconnecting")`. Borrar el `else` y el bloque `setTimeout(10s)`.
- [ ] En el `es.onerror` del SSE per-run (~743-756): mismo cambio — dejar solo `setSseStatus("reconnecting")`, borrar `else` y `setTimeout(10s)`.
- [ ] Borrar `const globalDisconnTimerRef = useRef(null)` (~1392) y los `clearTimeout(globalDisconnTimerRef.current)` en `onopen` (~1459-1462) y en el cleanup del `useEffect` de mount (~1604-1606).
- [ ] Borrar `const disconnectTimerRef = useRef(null)` (~698) y los `clearTimeout(disconnectTimerRef.current)` en `es.onopen` (~737-740) y en el cleanup del `useEffect` SSE per-run (~807-810).
- [ ] Borrar `const retryGlobal = useCallback(...)` (~1610-1613).
- [ ] Quitar la prop `onRetryGlobal` de la firma `const TopBar = ({ pipelines, runs, globalConnState, onRetryGlobal })` (~415) y del uso `<TopBar ... onRetryGlobal={retryGlobal} />` (~1894).
- [ ] Actualizar comentario `// hidden|live|reconnecting|disconnected` (~1390) → `// hidden|live|reconnecting`.
- [ ] Actualizar comentario `// idle | live | reconnecting | disconnected` (~697) → `// idle | live | reconnecting`.
- [ ] Borrar la regla CSS `.badge-disconn { ... }` (~137).
- [ ] `go build ./...` debe seguir pasando.
- [ ] Verificar que no quedan referencias residuales: `grep -n "disconnected\|badge-disconn\|disconnectTimerRef\|globalDisconnTimerRef\|retryGlobal\|onRetryGlobal" internal/dash/assets/dash.html` debe devolver vacio.

## Archivos afectados

- `internal/dash/assets/dash.html` — modificar — eliminar badges, handlers, refs, callbacks y CSS asociados al estado disconnected.

## Riesgos

- Si el setter `setGlobalConnState("reconnecting")` queda como unico path desde `onerror` y el `EventSource` no logra reabrir, la UI queda permanentemente en "reconnecting…" sin escalar a "disconnected". Es el comportamiento deseado segun la spec (reconexion silenciosa, sin ruido).
- Borrar `onRetryGlobal` cambia la firma publica de `TopBar`. No es un componente exportado fuera del archivo, asi que no debiera romper nada externo, pero hay que asegurarse de no dejar `retryGlobal` huerfano si algun otro caller lo usa (grep confirmo que no).
- CSS `.badge-disconn` no se referencia en otros archivos del repo (solo `dash.html`). Igualmente, si el validator detecta uso externo, dejarla y limitar el cambio al JSX/JS.

## Out of scope

- Cambiar el comportamiento de reconexion del `EventSource`.
- Tocar el badge `reconnecting…`, los badges `live` o cualquier otra UI del dash.
- Refactor general de los handlers SSE.
- Logging server-side o instrumentacion adicional.
- Eliminar otras clases CSS reutilizadas (`badge-failed`, `dot-failed`, etc).

## Asunciones tecnicas validadas

1. `internal/dash/assets/dash.html` se sirve embebido por el binario Go del dash; no hay paso de build para JSX/CSS — los cambios se reflejan al rebuild del binario.
2. No hay tests unitarios de UI del dash; la verificacion funcional la hace el validator inspeccionando que el dash siga cargando y que el binario compile.
3. `badge-disconn` y los refs/callbacks listados (`disconnectTimerRef`, `globalDisconnTimerRef`, `retryGlobal`, `onRetryGlobal`) se usan exclusivamente para el badge disconnected (verificado con `grep -n` antes de redactar el plan).
4. El `EventSource` nativo del browser ya reintenta automaticamente cuando `readyState === CONNECTING`; no se necesita logica manual de retry para mantener la conexion.

---

_Plan generado por `/hs-auto` a partir de #139 (sin labels harness)._
