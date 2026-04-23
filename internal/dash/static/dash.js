// dash.js — interacciones del dashboard que NO maneja htmx por sí solo.
// Específicamente: cerrar el modal con Esc o click sobre el backdrop, y el
// switcher de tabs (PR | Issue) del modal fused.
//
// El swap del modal en sí lo hace htmx (hx-get="/drawer/{id}" sobre cada
// card). Acá solo escuchamos eventos del body para reaccionar al modal
// montado en #modal-slot.

(function () {
  "use strict";

  // closeModal vacía el slot. Idempotente: si ya estaba cerrado no hace
  // nada raro. El nombre conserva la semántica del flujo aunque el wrapper
  // exterior pasó de "drawer" a "modal". También cierra cualquier
  // EventSource activo (ver streaming más abajo) — definido como var (no
  // function declaration) para poder llamar desde el closure de
  // cierre-de-stream antes de que se defina, y permitir el decorator
  // patrón si en el futuro hace falta.
  function closeModal() {
    closeStreamIfOpen();
    var slot = document.getElementById("modal-slot");
    if (slot) slot.innerHTML = "";
  }
  // closeStreamIfOpen se define abajo junto con el resto del código SSE.
  // Si por alguna razón se llama antes (no debería: los listeners se
  // registran en el mismo scope), no-op.
  function closeStreamIfOpen() {
    /* redefined below */
  }

  // Esc cierra el modal. Listener global; barato.
  // "/" focusea el input de filter (si no estamos ya escribiendo en un input).
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      // Si el foco está en un input (ej: filter), Escape lo blurrea; si no,
      // cierra el modal.
      var t = e.target;
      if (t instanceof HTMLInputElement || t instanceof HTMLTextAreaElement) {
        t.blur();
        return;
      }
      closeModal();
      return;
    }
    if (e.key === "/") {
      var active = document.activeElement;
      if (active instanceof HTMLInputElement || active instanceof HTMLTextAreaElement) return;
      var filter = document.querySelector(".dash-top .filter");
      if (filter) {
        e.preventDefault();
        filter.focus();
        filter.select();
      }
    }
  });

  // Tab switcher del modal (PR / Issue). Delegado en body porque el modal se
  // swappea dinámicamente con htmx — no podemos bindear al botón directo sin
  // re-bindear después de cada swap. stopPropagation evita que el click
  // bubblee al handler de "click backdrop" que cierra el modal.
  document.body.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    var tabBtn = t.closest(".drawer-tabs .tab");
    if (!tabBtn) return;
    e.stopPropagation();
    // El wrapper con data-tab ahora es .modal (no .drawer); las clases
    // internas drawer-* siguen igual.
    var modal = tabBtn.closest(".modal");
    if (!modal) return;
    modal.dataset.tab = tabBtn.dataset.tab || "pr";
  });

  // Click sobre el backdrop cierra el modal. Solo dispara si el target del
  // click es directamente .modal-backdrop — clicks adentro de .modal (sobre
  // el modal o cualquier hijo) no llegan acá porque el target es el hijo,
  // no el backdrop. Esto es justo lo que queremos: solo el backdrop "vacío"
  // cierra. No hace falta excluir explícitamente .card / .dash-top porque
  // ninguno de esos coincide con .modal-backdrop como target.
  document.body.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    if (t.classList && t.classList.contains("modal-backdrop")) {
      closeModal();
    }
  });

  // Countdown al próximo poll en el status-chip. El chip tiene
  // data-poll-interval (segundos) que usamos como baseline.
  //
  // Baseline del countdown: Date.now() en el cliente cuando HTMX terminó de
  // swappear /board — NO el data-last-ok-ms del server. El timestamp del
  // server marca cuándo terminó el refresh de gh, pero el cliente recibe
  // el swap 1-2s después (roundtrip + render). Usar el server-side hacía
  // que el primer tick post-swap arrancara con elapsed=1-2s y el countdown
  // saltara de "next in 15s" a "next in 13s" en 0.5s. Con Date.now() en el
  // cliente, el countdown arranca en 15 y baja monotonic de a uno por segundo.
  //
  // Solo aplica cuando el chip está en modo "ok" (tiene data-poll-interval);
  // en mock/stale/connecting el texto es estático.
  var clientLastOk = Date.now();
  document.body.addEventListener("htmx:afterOnLoad", function (e) {
    var path = e && e.detail && e.detail.requestConfig && e.detail.requestConfig.path;
    if (path === "/board") {
      clientLastOk = Date.now();
    }
  });
  setInterval(function () {
    var chip = document.getElementById("status-chip");
    if (!chip) return;
    var interval = parseInt(chip.dataset.pollInterval || "0", 10);
    if (!interval) return;
    var elapsed = Math.floor((Date.now() - clientLastOk) / 1000);
    var remaining = Math.max(0, interval - elapsed);
    var textEl = chip.querySelector(".chip-text");
    if (!textEl) return;
    textEl.textContent = remaining > 0 ? "next in " + remaining + "s" : "polling…";
  }, 1000);

  // ==================== Live log streaming (SSE) ====================
  //
  // Cuando el modal monta con data-entity=N, abrimos un EventSource a
  // /stream/N y appendeamos cada `event: line` al .live-log del
  // .drawer-logs-body visible. Hay hasta 2 .drawer-logs-body posibles en
  // el DOM (fused: pane-pr + pane-issue); en la práctica solo el pane-pr
  // tiene stream (el issue pane es solo body markdown). Pintamos TODOS
  // los .live-log del modal — el que no está visible simplemente queda
  // fuera de pantalla sin costo observable.
  //
  // Estado efímero por modal (cerramos al desmontar):
  //   - currentSource: el EventSource activo (null si no hay).
  //   - currentEntity: id del modal montado.
  //   - streamState: flags {paused, autoScroll} por-modal. Se resetean
  //     al desmontar.
  var currentSource = null;
  var currentEntity = null;
  var streamState = { paused: false, autoScroll: true };

  function fmtTime(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    var hh = String(d.getHours()).padStart(2, "0");
    var mm = String(d.getMinutes()).padStart(2, "0");
    var ss = String(d.getSeconds()).padStart(2, "0");
    return hh + ":" + mm + ":" + ss;
  }

  // classifyLine infiere la clase semántica (tool/ok/warn/err/info) a
  // partir de heurísticas sobre el texto. No parseamos stream-json —
  // solo miramos prefijos comunes que che loguea a stderr. Si no
  // matchea, caemos al default (texto plano, sin clase).
  function classifyLine(stream, text) {
    if (stream === "meta") return "info";
    if (!text) return "";
    // Tool use de claude: "[tool] ..." o "[edit] ...".
    if (/^\[(tool|edit|read|grep|bash|write|glob)\]/i.test(text)) return "tool";
    // Mensajes de éxito típicos: "ok", "PASS", "passed", "OK".
    if (/\b(passed|✓|ok\b)/i.test(text)) return "ok";
    // Errores / fail explícitos.
    if (/\b(error|fatal|failed|fail\b|panic)/i.test(text)) return "err";
    // Warnings.
    if (/\b(warn|warning|deprecated)/i.test(text)) return "warn";
    return "";
  }

  function appendLineToDOM(payload) {
    if (streamState.paused) return;
    var modal = document.querySelector(".modal");
    if (!modal) return;
    var buckets = modal.querySelectorAll(".drawer-logs-body .live-log");
    if (!buckets.length) return;
    var cls = classifyLine(payload.s, payload.x);
    var tStr = fmtTime(payload.t);
    buckets.forEach(function (el) {
      var row = document.createElement("div");
      var t = document.createElement("span");
      t.className = "t";
      t.textContent = tStr;
      row.appendChild(t);
      row.appendChild(document.createTextNode(" "));
      if (cls) {
        var span = document.createElement("span");
        span.className = cls;
        span.textContent = payload.x;
        row.appendChild(span);
      } else {
        row.appendChild(document.createTextNode(payload.x));
      }
      el.appendChild(row);
      // Ocultar el placeholder "sin logs todavía" apenas llega la 1ra línea.
      var empty = el.parentElement && el.parentElement.querySelector(".live-empty");
      if (empty) empty.style.display = "none";
      if (streamState.autoScroll) {
        var body = el.closest(".drawer-logs-body");
        if (body) body.scrollTop = body.scrollHeight;
      }
    });
  }

  function closeStream() {
    if (currentSource) {
      try { currentSource.close(); } catch (_) {}
    }
    currentSource = null;
    currentEntity = null;
    // Reset del estado para el próximo modal. El operador espera
    // auto-scroll activo por default cada vez que abre uno.
    streamState = { paused: false, autoScroll: true };
  }
  // Wireup para que closeModal (definido arriba) pueda cerrar el stream.
  // Reasignamos la stub de arriba con la implementación real.
  closeStreamIfOpen = closeStream;

  function openStream(entity) {
    closeStream();
    if (!entity) return;
    currentEntity = entity;
    var es;
    try {
      es = new EventSource("/stream/" + encodeURIComponent(entity));
    } catch (err) {
      // EventSource no disponible (muy viejo) o URL inválida — no hay
      // stream en vivo, pero el modal sigue funcional.
      return;
    }
    currentSource = es;
    es.addEventListener("line", function (ev) {
      var payload;
      try { payload = JSON.parse(ev.data); } catch (_) { return; }
      appendLineToDOM(payload);
    });
    es.addEventListener("done", function () {
      closeStream();
    });
    es.addEventListener("error", function () {
      // Errores transitorios: el browser reconecta solo. Si el stream
      // devolvió 404 (no hay flow para esa entity), el error se repite
      // hasta que cerremos — cerramos explícito para no martillar.
      if (es.readyState === EventSource.CLOSED) {
        closeStream();
      }
    });
  }

  // Controles del header del stream (auto-scroll / pause / clear).
  // Delegado en body porque el .ctl vive dentro del modal swappeado.
  document.body.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    var ctl = t.closest(".drawer-logs-hdr .ctl span");
    if (!ctl) return;
    var label = (ctl.textContent || "").toLowerCase();
    if (label.indexOf("auto-scroll") !== -1) {
      streamState.autoScroll = !streamState.autoScroll;
      ctl.classList.toggle("on", streamState.autoScroll);
      ctl.textContent = (streamState.autoScroll ? "● " : "○ ") + "auto-scroll";
    } else if (label === "pause" || label === "resume") {
      streamState.paused = !streamState.paused;
      ctl.textContent = streamState.paused ? "resume" : "pause";
      ctl.classList.toggle("on", streamState.paused);
    } else if (label === "clear") {
      var modal = document.querySelector(".modal");
      if (!modal) return;
      modal.querySelectorAll(".drawer-logs-body .live-log").forEach(function (el) {
        el.innerHTML = "";
      });
    }
  });

  // htmx:afterSwap — cuando el target es #modal-slot y el nuevo contenido
  // trae una .modal con data-entity, abrimos el stream. Si el slot queda
  // vacío (cierre del modal) cerramos el stream.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (!e.detail || !e.detail.target) return;
    if (e.detail.target.id !== "modal-slot") return;
    var modal = e.detail.target.querySelector(".modal");
    if (!modal) {
      // Slot vacío → modal cerrado.
      closeStream();
      return;
    }
    var entity = modal.getAttribute("data-entity");
    if (!entity) return;
    // Si el modal que acaba de montar es del mismo entity que ya tenemos
    // streamed, no hace falta reabrir (hx-post /action devuelve el mismo
    // drawer refrescado). Pero como el swap destruye el DOM viejo, el
    // .live-log actual está vacío — mejor reabrir para ver la historia
    // completa + futuras líneas.
    openStream(entity);
  });

  // closeModal (Esc / click backdrop / x) cierra el stream via la stub
  // closeStreamIfOpen definida al principio y reasignada a closeStream
  // acá — no hace falta decorar la función.

  // ==================== Auto-loop popover (Step 6) ====================
  //
  // El popover del auto-loop engine se llena lazy: el pill (.auto-loop-
  // toggle) hace hx-get="/loop" con target=#loop-popover. Cuando el
  // usuario clickea cualquier cosa fuera del popover o del pill, lo
  // cerramos vaciando #loop-popover. Las respuestas de los POST
  // (/loop/toggle, /loop/rule/...) incluyen un OOB del pill — htmx se
  // encarga de esa parte automáticamente.
  //
  // Esc también lo cierra (antes del modal close, porque si está abierto
  // el popover debe ir primero — como el modal en una ventana nativa).
  function closeLoopPopover() {
    var pop = document.getElementById("loop-popover");
    if (pop) pop.innerHTML = "";
  }

  document.body.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    var pop = document.getElementById("loop-popover");
    if (!pop) return;
    // Click dentro del popover: no cerrar (deja que htmx haga su swap).
    if (pop.contains(t)) return;
    // Click sobre el pill: no cerrar (sería re-abrir en el mismo click).
    // El hx-get del pill ya repoblará el popover si está vacío.
    if (t.closest(".auto-loop-toggle")) return;
    // Popover vacío: nada que cerrar.
    if (!pop.innerHTML.trim()) return;
    closeLoopPopover();
  });

  // Esc cierra popover si está abierto; sino cae al modal (ver handler
  // de arriba). Usamos capture para correr antes del listener del modal,
  // así un Esc cuando ambos están abiertos cierra primero el popover
  // (jerárquico — como una ventana nativa).
  document.addEventListener(
    "keydown",
    function (e) {
      if (e.key !== "Escape") return;
      var pop = document.getElementById("loop-popover");
      if (pop && pop.innerHTML.trim()) {
        e.stopPropagation();
        closeLoopPopover();
      }
    },
    true /* capture: corre antes del listener de body */
  );
})();
