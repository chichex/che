// Package lock implementa el mutex con TTL + heartbeat sobre la familia de
// labels `che:lock:<unix-nano>:<pid>-<host>` definida en
// `internal/pipelinelabels` (PRD §6.d).
//
// Modelo:
//
//   - Acquire: escanea los labels del ref, detecta cualquier `che:lock:*`
//     existente. Si el lock está vivo (timestamp + TTL > ahora), devuelve
//     ErrAlreadyLocked. Si está stale (timestamp viejo, dueño murió sucio),
//     lo borra y aplica uno nuevo. Si no había, aplica directo.
//   - Heartbeat: una goroutine refresca el timestamp (delete old + add new)
//     cada `HeartbeatInterval`. Mantiene el lock "vivo" durante runs largos.
//   - Release: borra el label actual y termina la goroutine.
//
// Diferencia con `internal/labels.Lock` (binario `che:locked`): este lock
// lleva timestamp + identidad del proceso, lo que permite detectar staleness
// (un lock de hace 10min sin heartbeat = el dueño murió). El binario
// `che:locked` sigue existiendo como mutex simple — los flows pueden usar
// los dos en simultáneo durante la transición; este paquete no toca el
// binario.
//
// El paquete NO depende de internal/labels para mantener la separación: la
// fuente de verdad del formato del label vive en internal/pipelinelabels
// (Parse/LockLabelAt) y la aplicación REST se hace acá directamente. Eso
// rompe el potencial ciclo `labels → lock → labels`.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chichex/che/internal/pipelinelabels"
)

// TTL es la ventana después de la cual un lock sin heartbeat se considera
// stale (dueño muerto). Default 5 minutos según PRD §6.d.
//
// Variable (no const) para que tests inyecten valores cortos sin sleeps
// largos.
var TTL = 5 * time.Minute

// HeartbeatInterval es la frecuencia con la que la goroutine refresca el
// timestamp del lock. Default 60s — tiene que ser sustancialmente menor
// que TTL para que un heartbeat perdido no arranque false-positive de
// stale-detection en otro proceso.
//
// Variable para tests.
var HeartbeatInterval = 60 * time.Second

// ErrAlreadyLocked es el sentinel que devuelve Acquire cuando hay un lock
// vivo (no stale) presente. Los callers lo mapean a ExitSemantic con
// mensaje accionable ("otro flow está corriendo, esperá o revisá").
var ErrAlreadyLocked = errors.New("ref already locked by another live process")

// ErrPostCheckFailed lo devuelve Acquire cuando logró aplicar su propio
// lock pero la re-list para detectar races falló. Distingue ese caso del
// éxito limpio para que el caller pueda decidir abortar (el lock no es
// confiable) o continuar con un warn. Wrappea el error subyacente para
// que `errors.Is/As` siga funcionando.
//
// Importante: cuando Acquire devuelve este error, el lock SÍ está aplicado
// en GitHub. El caller que aborta es responsable de invocar Release()
// sobre el handle adjunto (ver AcquirePostCheckFailedHandle helper si se
// agrega más adelante). En el wireup actual de runguard, se loggea warn
// y se sigue — preservando comportamiento pre-tie-break.
var ErrPostCheckFailed = errors.New("lock: re-list para detectar race lost falló")

// Handle es el resultado de un Acquire exitoso. Mantiene viva la goroutine
// de heartbeat hasta que el caller llama Release.
//
// Cero-valor no es válido (no tiene goroutine corriendo); usar Acquire para
// obtener un Handle inicializado.
type Handle struct {
	ref         string
	number      int
	pid         int
	host        string
	current     string // label aplicado actualmente
	mu          sync.Mutex
	stop        chan struct{}
	stopped     bool
	stopOnce    sync.Once
	now         func() time.Time // inyectable para tests del heartbeat
	ensureLabel func(label string) error
	addLabel    func(number int, label string) error
	delLabel    func(number int, label string) error
	listLbls    func(number int) ([]string, error)
	logErr      func(format string, args ...any) // opcional, para warnings del heartbeat
}

// Options ajusta el comportamiento de Acquire. Cero-valor es válido (usa
// defaults: time.Now, gh REST, sin logger).
type Options struct {
	// Now devuelve el "ahora" usado para timestamp + stale detection.
	// Default time.Now. Tests inyectan un clock fakeable.
	Now func() time.Time

	// PID identifica el proceso dueño del lock. Default os.Getpid().
	// Tests inyectan IDs determinísticos.
	PID int

	// Host identifica la máquina dueña. Default os.Hostname() o
	// "unknown-host". Tests inyectan strings fijos.
	Host string

	// EnsureLabel garantiza que el label exista en el repo antes del POST
	// (sin esto el POST crea el label con color default y pierde estilo).
	// Default `gh label create --force`. Tests stubean para evitar shell-out.
	EnsureLabel func(label string) error

	// AddLabel, DelLabel, ListLabels son los hooks REST. Defaults llaman
	// a `gh api`. Tests stubean para evitar shell-out.
	AddLabel    func(number int, label string) error
	DelLabel    func(number int, label string) error
	ListLabels  func(number int) ([]string, error)

	// LogErrf es el logger para warnings del heartbeat (errores no
	// fatales mientras refresca). Default no-op. Los flows lo wirean al
	// logger del run para no perderse los avisos.
	LogErrf func(format string, args ...any)
}

// Acquire intenta tomar el lock sobre `ref`. Si hay un lock previo:
//
//   - Vivo (timestamp + TTL > now): devuelve ErrAlreadyLocked.
//   - Stale (timestamp + TTL < now): lo borra y aplica uno nuevo.
//
// Si no había lock previo, aplica uno y arranca la goroutine de heartbeat.
//
// Race window: entre el list y el POST puede entrar otro proceso. GitHub
// trata el POST de label como idempotente (no devuelve error si ya existe
// uno con el mismo nombre, pero los nombres son únicos por timestamp/pid),
// y el caller que llega segundo va a ver el del primero al hacer su list.
// Para detectar la race, este código hace post-check después del POST: si
// detectamos otro lock vivo distinto al nuestro, abortamos y borramos el
// nuestro. No es CAS estricto (GitHub no lo expone) pero cubre el caso
// real (dos humanos lanzando che a la vez).
func Acquire(ref string, opts Options) (*Handle, error) {
	number, err := parseRefNumber(ref)
	if err != nil {
		return nil, fmt.Errorf("lock: parse ref %q: %w", ref, err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	pid := opts.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	host := opts.Host
	if host == "" {
		host = hostnameOrUnknown()
	}
	ensureLabel := opts.EnsureLabel
	if ensureLabel == nil {
		ensureLabel = ghEnsureLabel
	}
	addLabel := opts.AddLabel
	if addLabel == nil {
		addLabel = ghAddLabel
	}
	delLabel := opts.DelLabel
	if delLabel == nil {
		delLabel = ghDelLabel
	}
	listLabels := opts.ListLabels
	if listLabels == nil {
		listLabels = ghListLabels
	}

	// 1) Inspeccionar locks existentes.
	existing, err := listLabels(number)
	if err != nil {
		return nil, fmt.Errorf("lock: list labels for #%d: %w", number, err)
	}
	for _, l := range existing {
		p, perr := pipelinelabels.Parse(l)
		if perr != nil || p.Kind != pipelinelabels.KindLock {
			continue
		}
		// Lock encontrado: ¿stale o vivo?
		if now().Sub(p.Timestamp) < TTL {
			// Vivo — abortamos.
			return nil, fmt.Errorf("%w: lock %s (pid=%d host=%s, edad=%s)",
				ErrAlreadyLocked, l, p.PID, p.Host, now().Sub(p.Timestamp).Truncate(time.Second))
		}
		// Stale: borramos y seguimos.
		if err := delLabel(number, l); err != nil {
			return nil, fmt.Errorf("lock: borrar lock stale %s: %w", l, err)
		}
	}

	// 2) Aplicar lock nuevo.
	label := pipelinelabels.LockLabelAt(now(), pid, host)
	if err := ensureLabel(label); err != nil {
		return nil, fmt.Errorf("lock: ensure label %s: %w", label, err)
	}
	if err := addLabel(number, label); err != nil {
		return nil, fmt.Errorf("lock: apply lock %s: %w", label, err)
	}

	// Post-check de race: re-listar y resolver la carrera con tie-breaking
	// determinístico. Si vemos otro lock vivo en la re-list, comparamos
	// timestamps:
	//
	//   - Nuestro timestamp MENOR: ganamos (llegamos primero). Borramos el
	//     lock del otro y seguimos. El otro proceso, cuando haga su
	//     post-check, va a ver el nuestro como ganador y se va a retirar.
	//   - Nuestro timestamp MAYOR: el otro gana. Borramos el nuestro y
	//     devolvemos ErrAlreadyLocked.
	//   - Timestamps iguales: desempate por string-compare del segmento
	//     `<pid>-<host>` (orden total estable, libre de colisión salvo dos
	//     procesos con el mismo PID y hostname — caso patológico).
	//   - El otro lock no se puede parsear (timestamp inválido /
	//     pid-host malformado): tratado como "broken lock" → nosotros
	//     ganamos, lo borramos.
	//
	// Si la re-list falla con error, devolvemos ErrPostCheckFailed
	// envuelto: el lock nuestro está aplicado pero no podemos verificar
	// que seamos los únicos vivos. El caller decide qué hacer (abort vs
	// continue with warn). La señal NO es "OK" silencioso — eso anularía
	// completamente la mitigación de race si el segundo list falla por
	// red intermitente.
	existing2, listErr := listLabels(number)
	if listErr != nil {
		warnFn := opts.LogErrf
		if warnFn != nil {
			warnFn("post-check re-list falló para race detection: %v", listErr)
		}
		// Devolvemos error envuelto pero NO retiramos el lock — el caller
		// que reciba ErrPostCheckFailed puede decidir abort+release o
		// continuar (runguard hace lo segundo: warn y proceed).
		// Construimos un Handle parcial para que el caller pueda Release
		// si decide abortar.
		h := &Handle{
			ref:         ref,
			number:      number,
			pid:         pid,
			host:        host,
			current:     label,
			stop:        make(chan struct{}),
			stopOnce:    sync.Once{},
			now:         now,
			ensureLabel: ensureLabel,
			addLabel:    addLabel,
			delLabel:    delLabel,
			listLbls:    listLabels,
			logErr:      opts.LogErrf,
		}
		// No arrancamos heartbeat: el caller probablemente va a Release
		// inmediatamente. Si decide continuar, va a perder el heartbeat
		// — costo aceptable porque el lock simplemente expirará por TTL
		// si el proceso no llega a Release. La goroutine de heartbeat
		// nunca se lanzó, así que stop puede quedar abierto: la primera
		// llamada a Release la cierra (vía stopOnce) sin lanzar nada.
		return h, fmt.Errorf("%w: %v", ErrPostCheckFailed, listErr)
	}
	ourTS := now()
	ourPidHost := fmt.Sprintf("%d-%s", pid, host)
	var loserOthers []string // locks que perdieron contra nosotros (a borrar)
	for _, l := range existing2 {
		if l == label {
			continue
		}
		p, perr := pipelinelabels.Parse(l)
		if perr != nil {
			// No es un che:lock:* parseable. Si tiene el prefijo, lo
			// consideramos broken y nos lo llevamos puesto; si no lo
			// tiene, no es de nuestro paquete y lo ignoramos.
			if !strings.HasPrefix(l, pipelinelabels.PrefixLock) {
				continue
			}
			loserOthers = append(loserOthers, l)
			continue
		}
		if p.Kind != pipelinelabels.KindLock {
			continue
		}
		if now().Sub(p.Timestamp) >= TTL {
			// Stale: limpiamos pero no es contendor.
			loserOthers = append(loserOthers, l)
			continue
		}
		// Tie-break determinístico contra el otro lock vivo.
		theirPidHost := fmt.Sprintf("%d-%s", p.PID, p.Host)
		ourWins := false
		switch {
		case ourTS.Before(p.Timestamp):
			ourWins = true
		case p.Timestamp.Before(ourTS):
			ourWins = false
		default:
			// Timestamps iguales — desempate por string compare del
			// pid-host. El menor lexicográfico gana (orden estable).
			ourWins = ourPidHost < theirPidHost
		}
		if ourWins {
			loserOthers = append(loserOthers, l)
		} else {
			// El otro gana. Retiramos el nuestro y avisamos.
			if delErr := delLabel(number, label); delErr != nil {
				if opts.LogErrf != nil {
					opts.LogErrf("race lost: no se pudo borrar nuestro lock %s: %v", label, delErr)
				}
			}
			return nil, fmt.Errorf("%w: race lost contra %s (tie-break por timestamp)", ErrAlreadyLocked, l)
		}
	}
	// Si llegamos acá, ganamos todas las races. Borramos a los perdedores.
	for _, other := range loserOthers {
		if delErr := delLabel(number, other); delErr != nil {
			if opts.LogErrf != nil {
				opts.LogErrf("race won: no se pudo borrar lock perdedor %s: %v", other, delErr)
			}
		} else if opts.LogErrf != nil {
			opts.LogErrf("race won: borré lock perdedor %s (nuestro pidhost=%s)", other, ourPidHost)
		}
	}

	h := &Handle{
		ref:         ref,
		number:      number,
		pid:         pid,
		host:        host,
		current:     label,
		stop:        make(chan struct{}),
		now:         now,
		ensureLabel: ensureLabel,
		addLabel:    addLabel,
		delLabel:    delLabel,
		listLbls:    listLabels,
		logErr:      opts.LogErrf,
	}
	go h.heartbeatLoop()
	return h, nil
}

// Release detiene el heartbeat y borra el label actual. Idempotente: si ya
// fue liberado antes, es no-op.
//
// No devuelve error si la borrada del label falla por 404 (label ausente
// — alguien ya lo sacó). Para cualquier otro error, lo propaga; el caller
// típico lo loggea como warning porque el flow ya terminó.
func (h *Handle) Release() error {
	if h == nil {
		return nil
	}
	h.stopOnce.Do(func() {
		close(h.stop)
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil
	}
	h.stopped = true
	current := h.current
	if current == "" {
		return nil
	}
	if err := h.delLabel(h.number, current); err != nil {
		return fmt.Errorf("lock: release %s: %w", current, err)
	}
	h.current = ""
	return nil
}

// CurrentLabel devuelve el label que el handle considera "suyo" en este
// momento (puede haber cambiado por un heartbeat). Útil para tests que
// quieren chequear que el heartbeat efectivamente refrescó el timestamp.
func (h *Handle) CurrentLabel() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current
}

// heartbeatLoop refresca el timestamp del lock cada HeartbeatInterval
// hasta que stop se cierre. Cada tick: aplica un label nuevo (timestamp
// fresco) y borra el viejo. El orden importa — primero add, después
// remove — para que en la ventana entre las dos llamadas otro proceso que
// liste vea AMBOS y no asuma "no hay lock". Si alguna falla, loggea pero
// no aborta el loop (red transitoria, token recién rotado, etc.).
func (h *Handle) heartbeatLoop() {
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.tick()
		}
	}
}

// tick es un refresh atómico (en cuanto a estado interno): toma el lock,
// genera la nueva label, hace add+remove. Expone método separado para que
// los tests puedan llamarlo directo sin esperar al ticker.
func (h *Handle) tick() {
	h.mu.Lock()
	if h.stopped || h.current == "" {
		h.mu.Unlock()
		return
	}
	newLabel := pipelinelabels.LockLabelAt(h.now(), h.pid, h.host)
	if newLabel == h.current {
		// Mismo timestamp (resolución < nano improbable, pero defensa por
		// si el clock fakeable devuelve constante). Saltamos: no tiene
		// sentido borrar y volver a poner el mismo string.
		h.mu.Unlock()
		return
	}
	old := h.current
	h.current = newLabel
	h.mu.Unlock()

	if err := h.ensureLabel(newLabel); err != nil {
		h.warn("heartbeat: ensure %s: %v", newLabel, err)
		// Revertimos el current para que un retry futuro intente la
		// nueva versión, no la que fallamos en aplicar.
		h.mu.Lock()
		h.current = old
		h.mu.Unlock()
		return
	}
	if err := h.addLabel(h.number, newLabel); err != nil {
		h.warn("heartbeat: add %s: %v", newLabel, err)
		h.mu.Lock()
		h.current = old
		h.mu.Unlock()
		return
	}
	if err := h.delLabel(h.number, old); err != nil {
		h.warn("heartbeat: del %s: %v", old, err)
		// El nuevo está aplicado, el viejo quedó. Próximo tick lo limpia.
	}
}

// HeartbeatNow fuerza un refresh del lock fuera del ticker. Pensado SOLO
// para tests — en producción el ticker maneja el ritmo.
func (h *Handle) HeartbeatNow() {
	h.tick()
}

func (h *Handle) warn(format string, args ...any) {
	if h.logErr == nil {
		return
	}
	h.logErr(format, args...)
}

// hostnameOrUnknown duplica la función del paquete pipelinelabels sin
// exportarla — depender solo del paquete pipelinelabels via Parse/LockLabelAt
// mantiene la API mínima.
func hostnameOrUnknown() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}

// parseRefNumber acepta los mismos formatos que internal/labels.RefNumber
// pero duplicado acá para no introducir un import desde lock → labels (que
// crearía un ciclo si labels usa lock en el futuro). El subset de formatos
// soportado cubre todo lo que los flows pasan: número crudo, "#42",
// owner/repo#N, URLs.
func parseRefNumber(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, errors.New("empty ref")
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil {
		return n, nil
	}
	if i := strings.LastIndex(ref, "#"); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(ref[i+1:])); err == nil {
			return n, nil
		}
	}
	for _, seg := range []string{"/pull/", "/issues/"} {
		if i := strings.Index(ref, seg); i >= 0 {
			rest := ref[i+len(seg):]
			if j := strings.IndexAny(rest, "/?#"); j >= 0 {
				rest = rest[:j]
			}
			if n, err := strconv.Atoi(rest); err == nil {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("no se pudo extraer número del ref %q", ref)
}

// ghEnsureLabel garantiza que el label exista en el repo. Sin esto el
// POST de label crea una entry con color default y pasa por alto cualquier
// estilo. Idempotente.
func ghEnsureLabel(name string) error {
	cmd := exec.Command("gh", "label", "create", name, "--force")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh label create %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// ghAddLabel aplica un label a un issue/PR vía REST (uniforme issues/PRs).
func ghAddLabel(number int, label string) error {
	cmd := exec.Command("gh", "api",
		"-X", "POST",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels", number),
		"-f", "labels[]="+label,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api POST labels: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ghDelLabel borra un label vía REST. Tolerante a 404 (label ausente):
// alguien lo borró por race o el caller corre Release dos veces.
func ghDelLabel(number int, label string) error {
	cmd := exec.Command("gh", "api",
		"-X", "DELETE",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d/labels/%s", number, label),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	combined := string(out)
	if strings.Contains(combined, "Label does not exist") || strings.Contains(combined, "HTTP 404") {
		return nil
	}
	return fmt.Errorf("gh api DELETE labels/%s: %s", label, strings.TrimSpace(combined))
}

// ghListLabels devuelve la lista de labels actuales sobre un issue/PR.
func ghListLabels(number int) ([]string, error) {
	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/issues/%d", number),
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api issues/%d: %s", number, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var probe struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("parse issue labels: %w", err)
	}
	out2 := make([]string, 0, len(probe.Labels))
	for _, l := range probe.Labels {
		out2 = append(out2, l.Name)
	}
	return out2, nil
}
