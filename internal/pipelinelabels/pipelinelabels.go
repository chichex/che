// Package pipelinelabels genera y parsea los labels dinámicos derivados de
// un pipeline declarativo (`internal/pipeline`). Es la versión "v2" del
// modelo de labels: en lugar de la máquina de 9 estados hardcodeada de
// `internal/labels`, acá los labels se derivan del pipeline activo.
//
// Coexiste a propósito con `internal/labels`: este paquete es un shim para
// que el motor nuevo de pipelines (PRD #50, §6) pueda computar el set de
// labels esperados sin que los flows existentes se enteren. Mientras dure
// la migración, ambos paquetes conviven sin tocarse.
//
// Modelo (PRD §6 + §6.d):
//
//   - `che:state:<step>` — estado terminal del step. Ejemplo: `che:state:idea`,
//     `che:state:explore`. Hay un solo `che:state:*` aplicado al issue raíz a
//     la vez; cada transición lo reemplaza.
//   - `che:state:applying:<step>` — lock optimista mientras el step corre.
//     Reemplaza al `che:state:*` previo durante la ejecución y vuelve al
//     terminal al terminar OK (o rolea atrás si falla).
//   - `che:lock:<timestamp>:<pid>-<host>` — lock con heartbeat + TTL (PRD
//     §6.d). Identifica al proceso dueño del lock para detectar staleness.
//
// Este paquete NO aplica labels a GitHub: sólo los genera y parsea. La
// aplicación REST vive en `internal/labels` (que se reusará cuando el motor
// nuevo wire-up este shim).
package pipelinelabels

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chichex/che/internal/pipeline"
)

// Prefijos canónicos. Mantener exportados para que callers (engine, dash,
// drift detection) no inventen literales y para tests fuera del paquete.
//
// Importante: los prefijos llevan `:` final para que matchear "empieza con
// PrefixState" no acepte por error otras familias (ej. `che:state-foo`).
const (
	// PrefixState identifica labels de estado terminal de un step.
	// Forma: `che:state:<step>`.
	PrefixState = "che:state:"

	// PrefixApplying identifica labels de step en curso (lock optimista).
	// Forma: `che:state:applying:<step>`. Notar que también empieza con
	// PrefixState — el parser tiene que chequear este prefijo PRIMERO.
	PrefixApplying = "che:state:applying:"

	// PrefixLock identifica el lock con timestamp + pid + host (PRD §6.d).
	// Forma: `che:lock:<unix-nano>:<pid>-<host>`.
	PrefixLock = "che:lock:"
)

// Kind clasifica un label parseado. Los tres valores corresponden a las
// tres familias generadas por este paquete.
type Kind int

const (
	// KindUnknown es el zero value: el label no matchea ninguna familia
	// conocida. El parser devuelve este kind junto con un error para que
	// el caller distinga "no es nuestro" de "es nuestro pero malformado".
	KindUnknown Kind = iota

	// KindState = `che:state:<step>` (terminal).
	KindState

	// KindApplying = `che:state:applying:<step>` (transient).
	KindApplying

	// KindLock = `che:lock:<timestamp>:<pid>-<host>`.
	KindLock
)

// String devuelve un nombre legible del kind para mensajes de error y logs.
func (k Kind) String() string {
	switch k {
	case KindState:
		return "state"
	case KindApplying:
		return "applying"
	case KindLock:
		return "lock"
	default:
		return "unknown"
	}
}

// Parsed es el resultado de parsear un label. Los campos relevantes
// dependen del Kind:
//
//   - KindState / KindApplying → Step.
//   - KindLock → Timestamp, PID, Host.
//
// El zero value (Kind == KindUnknown) representa un label que no es de
// este paquete; el parser sólo devuelve Parsed con Kind != KindUnknown si
// no hubo error.
type Parsed struct {
	Kind Kind

	// Step es el nombre del step para KindState/KindApplying. Vacío en
	// KindLock.
	Step string

	// Timestamp es el instante en que se generó el lock (KindLock). Zero
	// para los otros kinds.
	Timestamp time.Time

	// PID es el process ID dueño del lock (KindLock). 0 para los otros.
	PID int

	// Host es el hostname dueño del lock (KindLock). Vacío para los otros.
	// Puede contener guiones (`-`), por eso el parser separa por el PRIMER
	// `-` del segmento `<pid>-<host>` (PID es numérico — todo lo que viene
	// después del primer `-` es host).
	Host string
}

// StateLabel devuelve el label terminal de un step: `che:state:<step>`.
//
// No valida `step`: el paquete `pipeline` ya restringe los nombres a
// `[a-z_][a-z0-9_]*` (ver Step.Name en pipeline.go); pasar un nombre con
// `:` rompería el round-trip parser → caller responsable.
func StateLabel(step string) string {
	return PrefixState + step
}

// ApplyingLabel devuelve el label transient de un step en ejecución:
// `che:state:applying:<step>`.
func ApplyingLabel(step string) string {
	return PrefixApplying + step
}

// Expected devuelve la lista plana de labels de estado esperados para un
// pipeline: por cada step, su `che:state:<step>` y su
// `che:state:applying:<step>`. NO incluye el lock (el lock es runtime, no
// del pipeline; ver LockLabel).
//
// Orden: por step en el orden del pipeline; dentro de cada step primero el
// terminal y luego el applying. Estable para que callers puedan compararlo
// bit-perfect en tests/golden.
//
// Uso típico: `che pipeline ensure-labels` para precrear todos los labels
// del repo de una sola, o drift detection comparando contra los labels
// realmente declarados en el repo.
func Expected(p pipeline.Pipeline) []string {
	out := make([]string, 0, len(p.Steps)*2)
	for _, s := range p.Steps {
		out = append(out, StateLabel(s.Name), ApplyingLabel(s.Name))
	}
	return out
}

// LockLabel genera un label de lock fresco con timestamp, PID y hostname
// del proceso actual. Forma: `che:lock:<unix-nano>:<pid>-<host>`.
//
// Timestamp en UnixNano garantiza que dos generaciones consecutivas no
// colisionen incluso en máquinas con reloj de baja resolución (la sección
// "tests del lock" del scope pide round-trip con timestamp distinto entre
// llamadas seguidas — UnixNano es suficiente sin necesidad de sleep en el
// test).
//
// Si os.Hostname() falla (poco común — readonly /etc/hostname o sandbox),
// usamos `unknown-host` como fallback. El lock pierde su valor de
// debugging pero sigue cumpliendo su rol de mutex; no abortamos toda la
// run por un hostname.
func LockLabel() string {
	return LockLabelAt(time.Now(), os.Getpid(), hostnameOrUnknown())
}

// LockLabelAt es la versión inyectable de LockLabel: permite tests
// determinísticos pasando timestamp, pid y host explícitos. No exportada
// para evitar uso en producción — el caller productivo debe usar
// LockLabel() para que el lock sea realmente único por proceso.
//
// Exportada con sufijo `At` para que el contrato (deterministic from
// inputs) sea evidente al lector. Ver lock_test.go para ejemplos.
func LockLabelAt(t time.Time, pid int, host string) string {
	if host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s%d:%d-%s", PrefixLock, t.UnixNano(), pid, host)
}

// hostnameOrUnknown wrappea os.Hostname() con fallback. Extraído para que
// tests no dependan del hostname real de la máquina.
func hostnameOrUnknown() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown-host"
	}
	return h
}

// Parse identifica un label como una de las tres familias del paquete y
// extrae sus campos.
//
// Returns:
//   - Parsed{Kind: KindState|KindApplying, Step: "..."} si matchea un
//     label de estado.
//   - Parsed{Kind: KindLock, Timestamp/PID/Host} si matchea un lock.
//   - Parsed{Kind: KindUnknown}, error no-nil si el label NO pertenece a
//     este paquete o está malformado.
//
// El error distingue dos casos:
//   - El label tiene un prefijo conocido pero no parsea (timestamp roto,
//     pid no numérico, etc.) → devuelve un error descriptivo.
//   - El label no tiene ninguno de nuestros prefijos → devuelve
//     ErrNotPipelineLabel para que el caller pueda silenciar el "no es
//     mío" sin hacer string match en el mensaje.
//
// Notar que el orden de chequeo es importante: PrefixApplying tiene que ir
// ANTES que PrefixState porque toda string que empieza con PrefixApplying
// también empieza con PrefixState (`che:state:applying:` ⊃ `che:state:`).
func Parse(label string) (Parsed, error) {
	switch {
	case strings.HasPrefix(label, PrefixApplying):
		step := strings.TrimPrefix(label, PrefixApplying)
		if step == "" {
			return Parsed{}, fmt.Errorf("pipelinelabels: applying label without step: %q", label)
		}
		return Parsed{Kind: KindApplying, Step: step}, nil

	case strings.HasPrefix(label, PrefixState):
		step := strings.TrimPrefix(label, PrefixState)
		if step == "" {
			return Parsed{}, fmt.Errorf("pipelinelabels: state label without step: %q", label)
		}
		return Parsed{Kind: KindState, Step: step}, nil

	case strings.HasPrefix(label, PrefixLock):
		return parseLock(label)
	}
	return Parsed{}, ErrNotPipelineLabel
}

// ErrNotPipelineLabel es el sentinel devuelto por Parse cuando el label no
// matchea ninguno de los 3 prefijos del paquete. Permite que callers
// detecten "no es mío" sin string-matching del mensaje:
//
//	if errors.Is(err, pipelinelabels.ErrNotPipelineLabel) { ... }
var ErrNotPipelineLabel = errors.New("pipelinelabels: not a pipeline label")

// parseLock extrae timestamp/pid/host de un label `che:lock:<ts>:<pid>-<host>`.
//
// Layout: PrefixLock + "<ts>:<pid>-<host>". Después de quitar el prefijo,
// quedan 2 segmentos separados por `:` (timestamp, pid-host). El segmento
// pid-host se separa por el PRIMER `-` (pid es numérico, host puede tener
// guiones).
func parseLock(label string) (Parsed, error) {
	body := strings.TrimPrefix(label, PrefixLock)
	colonIdx := strings.Index(body, ":")
	if colonIdx <= 0 || colonIdx == len(body)-1 {
		return Parsed{}, fmt.Errorf("pipelinelabels: malformed lock (expected che:lock:<ts>:<pid>-<host>): %q", label)
	}
	tsStr := body[:colonIdx]
	pidHost := body[colonIdx+1:]

	nanos, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return Parsed{}, fmt.Errorf("pipelinelabels: invalid lock timestamp %q in %q: %w", tsStr, label, err)
	}

	dashIdx := strings.Index(pidHost, "-")
	if dashIdx <= 0 || dashIdx == len(pidHost)-1 {
		return Parsed{}, fmt.Errorf("pipelinelabels: malformed lock pid-host (expected <pid>-<host>): %q", label)
	}
	pidStr := pidHost[:dashIdx]
	host := pidHost[dashIdx+1:]
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return Parsed{}, fmt.Errorf("pipelinelabels: invalid lock pid %q in %q: %w", pidStr, label, err)
	}

	return Parsed{
		Kind:      KindLock,
		Timestamp: time.Unix(0, nanos),
		PID:       pid,
		Host:      host,
	}, nil
}
