package output

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
	"github.com/muesli/termenv"
)

// writerSink renderiza Events a un io.Writer con ANSI opcional.
//
// Safe para uso concurrente: execute corre validadores async que pueden
// emitir desde multiples goroutines. El mutex serializa writes para que
// las lineas no se entrelacen a media salida.
type writerSink struct {
	w        io.Writer
	mu       sync.Mutex
	renderer *lipgloss.Renderer
}

// NewWriterSink construye un Sink que escribe a w.
//
// Auto-detecta TTY y respeta NO_COLOR/CI: si w no es terminal, o si
// NO_COLOR/CI estan seteados, desactiva ANSI. Esto hace safe redirigir
// a archivo o pipear sin ensuciar la salida con escape codes.
func NewWriterSink(w io.Writer) Sink {
	r := lipgloss.NewRenderer(w)
	if !shouldColor(w) {
		r.SetColorProfile(termenv.Ascii)
	}
	return &writerSink{w: w, renderer: r}
}

// Emit renderiza y escribe. Atomic via mutex (una linea por Emit).
func (s *writerSink) Emit(ev Event) {
	line := s.render(ev) + "\n"
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.WriteString(s.w, line)
}

func (s *writerSink) render(ev Event) string {
	sym := s.style(styleForLevel(ev.Level)).Render(symbolFor(ev.Level))
	bodyColored := s.style(styleForLevel(ev.Level)).Render(ev.Message)

	parts := []string{sym, bodyColored}

	if suffix := s.renderFields(ev.Fields); suffix != "" {
		parts = append(parts, suffix)
	}
	return strings.Join(parts, " ")
}

// renderFields compone los campos estructurados en un sufijo con orden
// predecible. Cada campo vacio se omite. Si no hay ningun campo, devuelve
// cadena vacia y el render no agrega espacio trailing.
func (s *writerSink) renderFields(f F) string {
	var out []string

	if f.Issue > 0 {
		out = append(out, s.style(styleNumber).Render(fmt.Sprintf("#%d", f.Issue)))
	}
	if f.PR > 0 {
		out = append(out, s.style(styleMuted).Render("PR")+" "+s.style(styleNumber).Render(fmt.Sprintf("#%d", f.PR)))
	}
	if len(f.Labels) > 0 {
		out = append(out, s.renderLabels(f.Labels))
	}
	if f.Iter > 0 {
		out = append(out, s.style(styleMuted).Render(fmt.Sprintf("iter=%d", f.Iter)))
	}
	if f.Agent != "" {
		out = append(out, s.style(styleAgent).Render("{"+f.Agent+"}"))
	}
	if f.Validator != "" {
		out = append(out, s.style(styleAgent).Render("{"+f.Validator+"}"))
	}
	if f.Attempt > 0 && f.Total > 0 {
		out = append(out, s.style(styleMuted).Render(fmt.Sprintf("(intento %d/%d)", f.Attempt, f.Total)))
	}
	if f.Verdict != "" {
		out = append(out, s.renderVerdict(f.Verdict))
	}
	if f.Cause != nil {
		out = append(out, s.style(styleMuted).Render("—")+" "+s.style(styleError).Render("error: "+f.Cause.Error()))
	}
	if f.Detail != "" {
		out = append(out, s.style(styleMuted).Render("("+f.Detail+")"))
	}
	if f.URL != "" {
		out = append(out, s.style(styleMuted).Render("·")+" "+s.style(styleURL).Render(f.URL))
	}

	return strings.Join(out, " ")
}

func (s *writerSink) renderLabels(labels []string) string {
	colored := make([]string, len(labels))
	for i, l := range labels {
		colored[i] = s.style(styleLabels).Render(l)
	}
	sep := s.style(styleMuted).Render(", ")
	open := s.style(styleMuted).Render("[")
	close := s.style(styleMuted).Render("]")
	return open + strings.Join(colored, sep) + close
}

func (s *writerSink) renderVerdict(v string) string {
	label := s.style(styleMuted).Render("verdict:")
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "approve":
		return label + " " + s.style(styleVerdictOK).Render(v)
	case "changes_requested", "changes-requested", "needs_human", "needs-human":
		return label + " " + s.style(styleVerdictKO).Render(v)
	default:
		return label + " " + s.style(styleMuted).Render(v)
	}
}

// style asocia el renderer del sink al estilo recibido. Como Style es
// value type, devuelve una copia con el renderer correcto (sin mutar el
// global).
func (s *writerSink) style(base lipgloss.Style) lipgloss.Style {
	return base.Renderer(s.renderer)
}

// shouldColor decide si habilitar ANSI para el writer dado.
//
// Reglas: NO_COLOR wins (https://no-color.org), CI desactiva color por
// convencion, y si w no es *os.File o no es TTY tampoco hay color.
func shouldColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CI") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd())
}

// Render es la funcion utility para obtener el string renderizado de un
// Event sin tener que construir un Sink. Utilizada por la TUI para reusar
// exactamente el mismo formato que el CLI cuando le conviene (ej. log
// line en runLog).
//
// El renderer asociado esta desacoplado de cualquier TTY: no escribe, y
// deja los estilos con el default profile global de lipgloss.
func Render(ev Event) string {
	sink := &writerSink{renderer: lipgloss.DefaultRenderer()}
	return sink.render(ev)
}
