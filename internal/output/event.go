package output

import "time"

// Level es la severidad del evento.
//
// No hay verbosity flags en che (decision cerrada): el Level solo define
// simbolo y color del render. Nada se filtra por nivel.
type Level int

const (
	LevelInfo    Level = iota // paso del flow, sin valor de exito
	LevelStep                 // sub-paso (dim) — reemplaza progress("…")
	LevelSuccess              // creacion exitosa, transicion OK, merge
	LevelWarn                 // best-effort failed, fallback aplicado
	LevelError                // error fatal o semantico
)

// F es el bag de metadata estructurada adjunto a cada evento. Todos los
// campos son opcionales; el renderer omite los vacios.
//
// Se llama F (no Fields) para que el call-site quede corto:
//
//	log.Success("creado", out.F{Issue: 47, Labels: []string{"type:feat"}})
type F struct {
	Issue     int      // renderiza "#47"
	PR        int      // renderiza "PR #7"
	Labels    []string // renderiza "[type:feat, ct:plan]"
	URL       string   // renderiza " · https://…"
	Agent     string   // renderiza "{opus}"
	Iter      int      // renderiza "iter=2"
	Verdict   string   // renderiza "verdict: approve"
	Validator string   // renderiza "{codex#1}"
	Attempt   int      // con Total, renderiza "(intento 2/3)"
	Total     int
	Cause     error  // renderiza " — error: <msg>"
	Detail    string // detalle libre al final
}

// Event es la unidad serializable que fluye entre flow y Sink.
//
// El CLI la renderiza con output.Render a una linea ANSI; la TUI la
// consume cruda y arma su propio render con lipgloss para integrarse
// con sus estilos (padding, truncate, etc.).
type Event struct {
	Time    time.Time
	Level   Level
	Message string
	Fields  F
}
