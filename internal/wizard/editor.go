package wizard

import (
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// editorFinishedMsg llega al Update cuando el subproceso del editor termina.
// Lleva el error de exec (nil si todo bien) — el reload del archivo se hace
// en el handler, no aca, para que la logica de "modelo pisado o no" viva en
// un solo lugar.
type editorFinishedMsg struct {
	err error
}

// resolveEditor devuelve el comando a usar como editor externo. Respeta
// $VISUAL y $EDITOR (en ese orden, igual que la convencion POSIX). Fallback
// a `vi` — esta en cualquier UNIX y en macOS por default.
func resolveEditor() string {
	if v := strings.TrimSpace(os.Getenv("VISUAL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("EDITOR")); v != "" {
		return v
	}
	return "vi"
}

// openEditorCmd construye un tea.Cmd que suspende el program, lanza el
// editor sobre path, y al terminar emite editorFinishedMsg. El callback de
// tea.ExecProcess no puede leer el archivo todavia: mejor que el reload viva
// en updateSummary asi el flujo "editor termino → reload → set state" queda
// en un solo lugar (mas facil de razonar y testear).
func openEditorCmd(path string) tea.Cmd {
	editor := resolveEditor()
	c := exec.Command("sh", "-c", editor+" \""+escapeShell(path)+"\"")
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorFinishedMsg{err: err}
	})
}

// escapeShell escapa comillas dobles dentro del path para que no rompan el
// `sh -c`. Pipelines paths viven en ~/.che/pipelines/<slug>.yaml — slug ya
// es ASCII alphanumerico + guiones (ver slug.go), pero el HOME del usuario
// puede tener espacios o comillas en macOS exotico. Sin esto el editor se
// abriria sobre el archivo equivocado.
func escapeShell(s string) string {
	out := make([]byte, 0, len(s)+4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\', '$', '`':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// summaryOpenInEditor es el handler de la tecla `y` en S3. Devuelve un
// tea.Cmd que suspende la TUI y abre el editor — el reload + transicion
// vuelve por handleEditorReturn.
func (m model) summaryOpenInEditor() (model, tea.Cmd) {
	if m.path == "" {
		// Defensa: sin path no hay archivo que abrir. No deberia pasar en
		// S3 (siempre llegamos aca tras Save), pero si pasa mostramos un
		// error inline coherente con el resto de S3.
		m.summaryErrs = []string{"no hay archivo de pipeline para abrir"}
		return m, nil
	}
	// Persistimos antes de soltar el control: el archivo en disco refleja
	// el ultimo estado del modelo. Si no, el editor abriria una version
	// vieja (o un archivo inexistente) y los cambios "anteriores en RAM"
	// quedarian fuera del editor.
	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageSummary
	m.pipeline.Status.StepIdx = 0
	m.pipeline.Status.StepMode = ""
	m.pipeline.Status.LastSavedAt = time.Now()
	if err := Save(m.path, m.pipeline); err != nil {
		m.summaryErrs = []string{"no se pudo guardar el draft: " + err.Error()}
		return m, nil
	}
	m.summaryErrs = nil
	return m, openEditorCmd(m.path)
}

// handleEditorReturn corre tras editorFinishedMsg: reload del archivo + pisa
// modelo si el parse OK, o muestra error inline manteniendo el modelo previo
// si el editor fallo / el YAML quedo invalido.
func (m model) handleEditorReturn(msg editorFinishedMsg) (model, tea.Cmd) {
	if msg.err != nil {
		m.summaryErrs = []string{"editor fallo: " + msg.err.Error()}
		return m, nil
	}
	loaded, err := Load(m.path)
	if err != nil {
		m.summaryErrs = []string{"no se pudo releer el archivo: " + err.Error()}
		return m, nil
	}
	// Pisamos el modelo en RAM con lo que quedo en disco. Decision: si el
	// archivo volvio sin status (el usuario lo borro a mano), igual lo
	// tratamos como draft mientras estemos en S3 — re-introducimos
	// status.stage=summary y re-persistimos para que el archivo refleje
	// "estamos en S3". El usuario sigue necesitando ctrl+s para ready-ficar.
	m.pipeline = loaded
	if m.summaryCursor < 0 {
		m.summaryCursor = 0
	}
	if m.summaryCursor >= len(m.pipeline.Steps) {
		m.summaryCursor = len(m.pipeline.Steps) - 1
	}
	if m.summaryCursor < 0 {
		m.summaryCursor = 0
	}
	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageSummary
	m.pipeline.Status.StepIdx = 0
	m.pipeline.Status.StepMode = ""
	m.pipeline.Status.LastSavedAt = time.Now()
	if err := Save(m.path, m.pipeline); err != nil {
		m.summaryErrs = []string{"no se pudo guardar el draft: " + err.Error()}
		return m, nil
	}
	m.summaryErrs = nil
	m.screen = ScreenSummary
	return m, nil
}
