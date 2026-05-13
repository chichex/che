package wizard

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// projectPipelinesSubdir es el subdir relativo al cwd donde viven los
// pipelines de scope project. Hay un eco intencional con
// `internal/pipelines.pipelinesSubdir` — la constante esta duplicada para
// evitar que wizard importe pipelines (cycle), pero el valor SIEMPRE debe
// coincidir. Si cambias uno, cambia el otro.
const projectPipelinesSubdir = ".che/pipelines"

// projectPathFor devuelve el path absoluto del archivo del pipeline en
// scope project. cwd="" devuelve "" — el caller debe degradar a global.
func projectPathFor(cwd, slug string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, projectPipelinesSubdir, slug+".yaml")
}

// resolvePathForScope calcula el path de save segun el scope del wizard.
// Falla limpio si el scope project se pidio sin cwd; el caller debe
// surfacear como errMsg.
func (m model) resolvePathForScope(slug string) (string, error) {
	if m.scope == WizardScopeProject {
		path := projectPathFor(m.cwd, slug)
		if path == "" {
			return "", fmt.Errorf("scope project no disponible (cwd vacio)")
		}
		return path, nil
	}
	return PathFor(m.homeDir, slug)
}

// updateScope handla teclas en ScreenScope (selector project vs global).
func (m model) updateScope(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return m.openCancel(ScreenScope)
	case "esc":
		// volver a S1, mantener buffers.
		m.screen = ScreenInfo
		m.errMsg = ""
		return m, nil
	case "up", "k", "left", "h":
		if m.scope == WizardScopeProject {
			m.scope = WizardScopeGlobal
		}
		return m, nil
	case "down", "j", "right", "l":
		if m.scope == WizardScopeGlobal {
			m.scope = WizardScopeProject
		}
		return m, nil
	case "1":
		m.scope = WizardScopeGlobal
		return m.commitScopeAndAdvance()
	case "2":
		m.scope = WizardScopeProject
		return m.commitScopeAndAdvance()
	case "tab":
		// tab toggle entre las dos opciones.
		if m.scope == WizardScopeGlobal {
			m.scope = WizardScopeProject
		} else {
			m.scope = WizardScopeGlobal
		}
		return m, nil
	case "enter", "ctrl+s", " ":
		return m.commitScopeAndAdvance()
	}
	return m, nil
}

// commitScopeAndAdvance materializa la decision de scope: calcula path,
// detecta colision, persiste, y avanza a S2. Si el scope project se
// pidio sin cwd o el slug no se puede resolver, marca errMsg y vuelve a
// ScreenInfo.
func (m model) commitScopeAndAdvance() (model, tea.Cmd) {
	name := strings.TrimSpace(m.nameInput.Value())
	slug := Slug(name)
	if slug == "" {
		// no deberia pasar (S1 ya valido), pero defensive: volver a S1.
		m.errMsg = "nombre invalido"
		m.screen = ScreenInfo
		return m, nil
	}

	path, err := m.resolvePathForScope(slug)
	if err != nil {
		m.errMsg = err.Error()
		// el usuario tiene que elegir otra cosa; volver a la pantalla.
		m.scope = WizardScopeGlobal
		return m, nil
	}

	exists, err := Exists(path)
	if err != nil {
		m.errMsg = "no se pudo chequear el archivo: " + err.Error()
		return m, nil
	}
	if exists {
		m.collisionCursor = CollisionOverwrite
		m.screen = ScreenCollision
		return m, nil
	}

	m.path = path
	m.pipeline.Name = name
	m.pipeline.Description = strings.TrimSpace(m.descInput.Value())
	m.pipeline.Status = &Status{
		Stage:       StageInfo,
		LastSavedAt: time.Now(),
	}
	if err := Save(m.path, m.pipeline); err != nil {
		m.errMsg = "no se pudo guardar: " + err.Error()
		return m, nil
	}
	m.errMsg = ""
	return m.enterStepCreate(0)
}

// viewScope renderea ScreenScope: dos opciones (global, project) con la
// seleccionada destacada. El path resuelto se muestra dimmed para que el
// usuario sepa exactamente donde se guarda.
func (m model) viewScope() string {
	var b strings.Builder
	b.WriteString(breadcrumb("Create pipeline", "paso 1/3 · scope"))
	b.WriteString("\n\n")
	b.WriteString("Donde guardar este pipeline?")
	b.WriteString("\n\n")

	name := strings.TrimSpace(m.nameInput.Value())
	slug := Slug(name)

	globalPath, _ := PathFor(m.homeDir, slug)
	projectPath := projectPathFor(m.cwd, slug)

	options := []struct {
		idx   WizardScope
		digit string
		label string
		hint  string
		path  string
	}{
		{WizardScopeGlobal, "1", "global", "disponible desde cualquier cwd", globalPath},
		{WizardScopeProject, "2", "project", "solo desde este proyecto (cwd-local)", projectPath},
	}
	for _, o := range options {
		marker := "  "
		labelLine := o.digit + ". " + o.label
		if m.scope == o.idx {
			marker = "> "
			labelLine = selectedItem.Render(labelLine)
		}
		row := marker + labelLine + "  " + dimStyle.Render(o.hint)
		if o.path != "" {
			row += "\n      " + dimStyle.Render(o.path)
		} else if o.idx == WizardScopeProject {
			row += "\n      " + dimStyle.Render("(cwd no disponible — no se puede usar este scope)")
		}
		b.WriteString(row)
		b.WriteString("\n\n")
	}

	if m.errMsg != "" {
		b.WriteString(errorStyle.Render("✗ " + m.errMsg))
		b.WriteString("\n\n")
	}

	b.WriteString(hintStyle.Render("↑/↓ navegar · enter elegir · esc volver"))
	b.WriteString("\n")
	return b.String()
}
