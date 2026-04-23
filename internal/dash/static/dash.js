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
  // exterior pasó de "drawer" a "modal".
  function closeModal() {
    var slot = document.getElementById("modal-slot");
    if (slot) slot.innerHTML = "";
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

  // htmx:afterSwap — antes acá sincronizábamos body[data-selected] para
  // pintar la card abierta. Sin highlight (con backdrop la selección visual
  // sobra), el handler queda muy minimalista; lo dejamos por si en el futuro
  // queremos engancharnos al swap del modal. Solo nos interesa cuando el
  // target es #modal-slot.
  document.body.addEventListener("htmx:afterSwap", function (e) {
    if (!e.detail || !e.detail.target) return;
    if (e.detail.target.id !== "modal-slot") return;
    // No-op por ahora. Hook reservado para futuras integraciones (por
    // ejemplo, focus management o analytics).
  });
})();
