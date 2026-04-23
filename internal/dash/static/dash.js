// dash.js — interacciones del dashboard que NO maneja htmx por sí solo.
// Específicamente: sincronizar el atributo body[data-selected] con la card
// activa (para el highlight CSS), y cerrar el drawer con Esc o click afuera.
//
// El swap del drawer en sí lo hace htmx (hx-get="/drawer/{id}" sobre cada
// card). Acá solo escuchamos eventos del body para mantener data-selected
// alineado con lo que esté swappeado en #drawer-slot.

(function () {
  "use strict";

  // closeDrawer vacía el slot y limpia el atributo de selección. Idempotente:
  // si ya estaba cerrado no hace nada raro.
  function closeDrawer() {
    var slot = document.getElementById("drawer-slot");
    if (slot) slot.innerHTML = "";
    document.body.removeAttribute("data-selected");
  }

  // Esc cierra el drawer. Listener global; barato.
  // "/" focusea el input de filter (si no estamos ya escribiendo en un input).
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") {
      // Si el foco está en un input (ej: filter), Escape lo blurrea; si no, cierra drawer.
      var t = e.target;
      if (t instanceof HTMLInputElement || t instanceof HTMLTextAreaElement) {
        t.blur();
        return;
      }
      closeDrawer();
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

  // Tab switcher del drawer (PR / Issue). Delegado en body porque el drawer
  // se swappea dinámicamente con htmx — no podemos bindear al botón directo
  // sin re-bindear después de cada swap. stopPropagation evita que el click
  // bubblee al handler de "click afuera" que cierra el drawer.
  document.body.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    var tabBtn = t.closest(".drawer-tabs .tab");
    if (!tabBtn) return;
    e.stopPropagation();
    var drawer = tabBtn.closest(".drawer");
    if (!drawer) return;
    drawer.dataset.tab = tabBtn.dataset.tab || "pr";
  });

  // Click afuera cierra. Consideramos "afuera" todo lo que NO sea:
  //   - una card (.card) — el click sobre otra card abre esa otra,
  //     no debe cerrar primero.
  //   - el drawer mismo (#drawer-slot) — clicks dentro del drawer
  //     (botones, scroll, etc.) no deben cerrar.
  //   - la topbar (.dash-top) — interacciones con filtros/toggle no
  //     deben cerrar el drawer.
  //
  // Usamos `capture: false` (default) y dejamos que htmx procese sus
  // hx-* primero; el handler de cierre solo dispara cuando el target no
  // está dentro de las zonas "vivas".
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!(t instanceof Element)) return;
    if (t.closest(".card")) return;
    if (t.closest("#drawer-slot")) return;
    if (t.closest(".dash-top")) return;
    closeDrawer();
  });

  // Countdown al próximo poll en el status-chip. El chip tiene data-last-ok-ms
  // (unix ms del último poll exitoso) y data-poll-interval (segundos). Cada
  // segundo calculamos remaining = interval - (now - lastOk) y lo rendereamos
  // en .chip-text. Cuando remaining llega a 0 mostramos "polling…" hasta que
  // htmx swappee con los data-attrs frescos.
  //
  // Solo aplica cuando el chip está en modo "ok" (tiene los data-attrs);
  // en mock/stale/connecting el texto es estático.
  setInterval(function () {
    var chip = document.getElementById("status-chip");
    if (!chip) return;
    var lastOk = parseInt(chip.dataset.lastOkMs || "0", 10);
    var interval = parseInt(chip.dataset.pollInterval || "0", 10);
    if (!lastOk || !interval) return;
    var elapsed = Math.floor((Date.now() - lastOk) / 1000);
    var remaining = Math.max(0, interval - elapsed);
    var textEl = chip.querySelector(".chip-text");
    if (!textEl) return;
    textEl.textContent = remaining > 0 ? "next in " + remaining + "s" : "polling…";
  }, 1000);

  // Cuando htmx termina de swappear el drawer, sincronizamos data-selected
  // con el id que vino del hx-trigger. Lo hacemos vía evento custom de htmx
  // en vez de hx-on::after-request inline para que cards futuras (cuando el
  // poller las re-renderice) no necesiten repetir el handler en cada tag.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (!e.detail || !e.detail.target) return;
    if (e.detail.target.id !== "drawer-slot") return;
    var slot = e.detail.target;
    // Si el swap dejó el slot vacío (caso /drawer/close) limpiamos selección.
    if (!slot.innerHTML.trim()) {
      document.body.removeAttribute("data-selected");
      return;
    }
    // El partial de drawer setea data-entity en su root; lo levantamos.
    var root = slot.querySelector("[data-entity]");
    if (root && root.dataset.entity) {
      document.body.dataset.selected = root.dataset.entity;
    }
  });
})();
