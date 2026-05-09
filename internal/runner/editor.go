package runner

import (
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// editorReturnedMsg llega tras tea.ExecProcess de R4/RF cuando el usuario
// cierra $EDITOR / $PAGER. err == nil si el subproceso salio limpio. Lo
// handleamos en updateDone / updateFailed con un no-op (la TUI ya volvio al
// frente sola); existe como tipo distinto para no chocar con el
// editorFinishedMsg del wizard si en el futuro hay un program compartido.
type editorReturnedMsg struct {
	err error
}

// resolveEditor devuelve el editor a usar para abrir result.yaml en R4.
// Convencion POSIX: $VISUAL > $EDITOR > vi. Mismo criterio que el wizard
// (internal/wizard/editor.go) — duplicado aca para evitar el import cruzado.
func resolveEditor() string {
	if v := strings.TrimSpace(os.Getenv("VISUAL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("EDITOR")); v != "" {
		return v
	}
	return "vi"
}

// resolvePager devuelve el pager a usar para abrir stdout.log / stderr.log
// desde R4/RF. Convencion POSIX: $PAGER > less. Si en el futuro alguien
// quiere `bat` o `most`, $PAGER lo cubre — mantenemos el fallback en `less`
// porque viene en cualquier UNIX/macOS por default.
func resolvePager() string {
	if v := strings.TrimSpace(os.Getenv("PAGER")); v != "" {
		return v
	}
	return "less"
}

// openInEditorCmd construye un tea.Cmd que suspende el program y lanza
// $EDITOR sobre path. Al volver emite editorReturnedMsg.
//
// Si path no existe, igual lanzamos el editor — la mayoria de los editores
// crean el archivo nuevo. result.yaml siempre debe existir cuando R4 esta
// activo (writeStepResult corre por step), pero el guard mantiene el path
// resiliente a races ("borraron el run dir mid-pantalla").
func openInEditorCmd(path string) tea.Cmd {
	editor := resolveEditor()
	c := exec.Command("sh", "-c", editor+" \""+escapeShell(path)+"\"")
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorReturnedMsg{err: err}
	})
}

// openInPagerCmd construye un tea.Cmd que suspende el program y lanza $PAGER
// sobre path. Al volver emite editorReturnedMsg (mismo tipo que editor — el
// caller no necesita distinguir, ambos son "TUI volvio del fork").
func openInPagerCmd(path string) tea.Cmd {
	pager := resolvePager()
	c := exec.Command("sh", "-c", pager+" \""+escapeShell(path)+"\"")
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorReturnedMsg{err: err}
	})
}

// escapeShell escapa los caracteres especiales del path para que no rompan
// el `sh -c`. Mismo criterio que internal/wizard/editor.go.escapeShell.
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
