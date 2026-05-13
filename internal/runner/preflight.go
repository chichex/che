package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/chichex/che/internal/repoctx"
	"github.com/chichex/che/internal/skills"
	"github.com/chichex/che/internal/wizard"
)

// PreflightStatus es el estado de un row del checklist R2. Pending mientras
// se evalua; OK / Fail / Warn al terminar. Warn se reserva para chequeos
// no-bloqueantes (disk space < 100 MB) — el usuario puede continuar via
// confirm modal.
type PreflightStatus int

const (
	PreflightPending PreflightStatus = iota
	PreflightOK
	PreflightFail
	PreflightWarn
)

// PreflightCheck es un row del checklist. Label se renderiza tal cual; Remedy
// es la linea de hint que aparece debajo cuando Status != OK (ver mockup en
// docs/pipeline-execution-flow.html, seccion R2). Para Pending dejamos
// Remedy vacio — el icono ⏳ + label alcanza para mostrar el chequeo en curso.
type PreflightCheck struct {
	Label  string
	Status PreflightStatus
	Remedy string
}

// minDiskBytes es el umbral por debajo del cual el chequeo de disco emite
// warning amarillo (no bloquea). El doc fija 100 MB libres en ~/.che/runs.
const minDiskBytes = 100 * 1024 * 1024

// preflightCmdTimeout acota cuanto puede tardar `gh auth status`. El
// chequeo es informativo: si gh esta colgado preferimos fallar rapido y
// ofrecer retry antes que freezear el TUI.
const preflightCmdTimeout = 5 * time.Second

// Funciones swappables para tests. El default usa los binarios reales (en
// los e2e el harness symlinkea gh / claude / etc a chefake; ahi tampoco
// hace falta tocar). Los tests unitarios pueden reemplazar punto por punto
// para ejercitar fail / warn / ok sin armar fixtures de filesystem reales.
var (
	lookPathFn   = exec.LookPath
	detectSkills = func() []skills.CLI { return skills.Detect("") }
	ghAuthFn     = defaultGhAuth
	diskFreeFn   = defaultDiskFree
)

// defaultGhAuth corre `gh auth status` con timeout corto. Devuelve ok=true
// si el exit es 0 (gh entiende que hay una sesion activa). Cualquier otro
// resultado es fail; el stderr/stdout no nos interesan aca — el remedio
// del row R2 es siempre el mismo ("gh auth login").
func defaultGhAuth() bool {
	ctx, cancel := context.WithTimeout(context.Background(), preflightCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "status")
	return cmd.Run() == nil
}

// defaultDiskFree devuelve los bytes libres en el filesystem que contiene
// path. Si statfs falla (path inexistente, etc) devuelve 0 — el caller lo
// trata como "no se pudo medir, mejor warnear".
func defaultDiskFree(path string) uint64 {
	var stat syscall.Statfs_t
	// Subimos al primer ancestro que existe — ~/.che/runs probablemente
	// no exista todavia (lo crea H4 al primer run); statfs igual te dice
	// el espacio del filesystem montado en el padre.
	probe := path
	for {
		if probe == "" || probe == "/" {
			probe = "/"
			break
		}
		if _, err := os.Stat(probe); err == nil {
			break
		}
		probe = filepath.Dir(probe)
	}
	if err := syscall.Statfs(probe, &stat); err != nil {
		return 0
	}
	// Bavail = bloques libres para el usuario unprivileged. Multiplicar
	// por Bsize (size del bloque) da bytes libres. Cast a uint64 protege
	// contra plataformas donde Bsize es int32 (linux) vs uint32 (darwin).
	return uint64(stat.Bavail) * uint64(stat.Bsize) //nolint:unconvert // platform-dependent types
}

// runDirForCheck es el path que pasamos a diskFreeFn. Lo extraemos para
// que tests de runPreflightChecks sepan exactamente que path se chequea
// sin tener que duplicar la logica de UserHomeDir.
func runDirForCheck() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir()
	}
	return filepath.Join(home, ".che", "runs")
}

// buildPreflightChecks arma el checklist segun lo que pide el pipeline.
// Devuelve todos los rows con Status=PreflightPending — runPreflightChecks
// los corre y devuelve la lista resuelta. Separar build/run hace que el
// render pueda mostrar ⏳ por algunos ms antes del primer ✓/✗ aun cuando
// la ejecucion completa toma <50ms (sin esto el usuario nunca ve el
// "estado animado" que el doc pide).
//
// Nota: H3 corre el chequeo sincronicamente al entrar a R2 — el doc admite
// que para v1 los chequeos son baratos (LookPath + filesystem reads + un
// gh auth status); el render "animado" se materializa solo cuando el
// usuario presiona `r` para reintentar (volvemos a Pending y re-render).
func buildPreflightChecks(p wizard.Pipeline, inputKind, inputValue string) []PreflightCheck {
	checks := []PreflightCheck{}

	// CLI installed: un row por cada CLI distinto en steps[].cli y
	// steps[].validator.cli. Orden estable para que tests no dependan
	// de orden de iteracion de map.
	clis := distinctCLIs(p)
	for _, c := range clis {
		checks = append(checks, PreflightCheck{
			Label:  fmt.Sprintf("cli %s instalado", c),
			Status: PreflightPending,
		})
	}

	// Skill exists: un row por cada step / validator con kind=skill. La
	// label incluye la skill y el cli para que el remedio "instalar la
	// skill" sea inequivoco cuando un mismo nombre vive en dos CLIs.
	for _, s := range distinctSkillRefs(p) {
		checks = append(checks, PreflightCheck{
			Label:  fmt.Sprintf("skill %s en %s", s.Skill, s.CLI),
			Status: PreflightPending,
		})
	}

	// Model whitelist: un row por cada (cli, model) declarado explicito en
	// step.Model o step.Validator.Model. Rechaza modelos desconocidos por
	// CLI ANTES de spawnear (issue #142). Steps sin `model:` no aportan
	// rows aca — caen al default por CLI sin chequeo.
	for _, r := range distinctModelRefs(p) {
		checks = append(checks, PreflightCheck{
			Label:  fmt.Sprintf("model %s para cli %s", r.Model, r.CLI),
			Status: PreflightPending,
		})
	}

	// gh auth + git repo context: ambos solo aplican si algun step usa
	// input pr|issue. El row de repo aparece antes que el de auth porque
	// "no estas en un repo" es un fail mas estructural que "no estas
	// logueado" — si fallan los dos, el usuario lee la causa raiz primero.
	if pipelineNeedsGh(p) {
		checks = append(checks, PreflightCheck{
			Label:  "git repo context",
			Status: PreflightPending,
		})
		checks = append(checks, PreflightCheck{
			Label:  "gh auth status",
			Status: PreflightPending,
		})
	}

	// File readable: re-check defensivo si el input del step 0 fue file.
	// El archivo pudo desaparecer entre R1 y R2 (segun la "Tabla de
	// errores" del doc). Skipeamos si inputKind != file o inputValue ""
	// (por las dudas; R1 ya valida no-vacio).
	if inputKind == wizard.InputFile && inputValue != "" {
		checks = append(checks, PreflightCheck{
			Label:  fmt.Sprintf("file readable: %s", inputValue),
			Status: PreflightPending,
		})
	}

	// Disk space siempre va al final — es el unico chequeo "warning, no
	// fail" del set. Tener un orden fijo simplifica los asserts en tests.
	checks = append(checks, PreflightCheck{
		Label:  fmt.Sprintf("disk space ≥ %d MB en ~/.che/runs", minDiskBytes/(1024*1024)),
		Status: PreflightPending,
	})

	return checks
}

// skillRef es una tupla skill+cli — sirve para ordenar / deduplicar sin
// concatenar strings. distinctSkillRefs garantiza que no aparezcan dos
// rows iguales si dos steps pisan la misma skill.
type skillRef struct {
	CLI   string
	Skill string
}

// distinctCLIs recorre todos los steps y validators y devuelve la lista
// ordenada de CLIs distintos no-vacios. Sirve para no chequear el mismo
// binario dos veces cuando un pipeline usa claude para 5 steps.
func distinctCLIs(p wizard.Pipeline) []string {
	seen := map[string]struct{}{}
	for _, st := range p.Steps {
		if st.CLI != "" {
			seen[st.CLI] = struct{}{}
		}
		if st.Validator != nil && st.Validator.CLI != "" {
			seen[st.Validator.CLI] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// modelRef es una tupla cli+model declarada en el YAML (step.Model o
// step.Validator.Model). El preflight la usa para chequear que el modelo
// matchea el whitelist por CLI ANTES de spawnear (issue #142).
type modelRef struct {
	CLI   string
	Model string
}

// distinctModelRefs deduplica + ordena los pares (cli, model) para steps y
// validators que declaran `model:` explicito. Steps con model=="" caen al
// default por CLI y no necesitan validacion. Para cli=="" defensivamente
// skipeamos — IsValid del wizard ya deberia haberlo rechazado.
func distinctModelRefs(p wizard.Pipeline) []modelRef {
	seen := map[modelRef]struct{}{}
	for _, st := range p.Steps {
		if st.CLI != "" && strings.TrimSpace(st.Model) != "" {
			seen[modelRef{CLI: st.CLI, Model: st.Model}] = struct{}{}
		}
		if st.Validator != nil && st.Validator.CLI != "" && strings.TrimSpace(st.Validator.Model) != "" {
			seen[modelRef{CLI: st.Validator.CLI, Model: st.Validator.Model}] = struct{}{}
		}
	}
	out := make([]modelRef, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CLI != out[j].CLI {
			return out[i].CLI < out[j].CLI
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// distinctSkillRefs deduplica + ordena los pares (cli, skill) para los
// steps / validators con kind=skill. Si un step kind=skill no tiene CLI
// asociada (caso defensive: deberia haberlo rechazado IsValid), lo
// skipeamos para no inventar una row sin sentido.
func distinctSkillRefs(p wizard.Pipeline) []skillRef {
	seen := map[skillRef]struct{}{}
	for _, st := range p.Steps {
		if st.Kind == wizard.KindSkill && st.CLI != "" && st.Content != "" {
			seen[skillRef{CLI: st.CLI, Skill: st.Content}] = struct{}{}
		}
		if st.Validator != nil && st.Validator.Kind == wizard.KindSkill && st.Validator.CLI != "" && st.Validator.Content != "" {
			seen[skillRef{CLI: st.Validator.CLI, Skill: st.Validator.Content}] = struct{}{}
		}
	}
	out := make([]skillRef, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CLI != out[j].CLI {
			return out[i].CLI < out[j].CLI
		}
		return out[i].Skill < out[j].Skill
	})
	return out
}

// pipelineNeedsGh chequea si algun step usa input pr o issue. Si si, R2
// agrega el row de gh auth status; si no, lo skipea (no tiene sentido
// pedir auth para un pipeline que no toca github).
func pipelineNeedsGh(p wizard.Pipeline) bool {
	for _, st := range p.Steps {
		if st.Input == wizard.InputPR || st.Input == wizard.InputIssue {
			return true
		}
	}
	return false
}

// runPreflightChecks itera sobre el set Pending y resuelve cada uno. Devuelve
// una lista nueva (no muta el slice de entrada) — asi el caller puede
// guardar el "snapshot Pending" si quiere renderizar el estado intermedio.
func runPreflightChecks(checks []PreflightCheck, p wizard.Pipeline, inputKind, inputValue string) []PreflightCheck {
	out := make([]PreflightCheck, len(checks))
	copy(out, checks)

	clis := detectSkills()
	cliInstalled := map[string]bool{}
	cliSkills := map[string]map[string]bool{}
	for _, c := range clis {
		cliInstalled[c.Name] = c.Installed
		set := map[string]bool{}
		for _, s := range c.Skills {
			set[s.Name] = true
		}
		cliSkills[c.Name] = set
	}

	for i, c := range out {
		out[i] = resolveCheck(c, p, inputKind, inputValue, cliInstalled, cliSkills)
	}
	return out
}

// resolveCheck despacha por el prefijo del label — la pesadez de hacer un
// switch sobre prefix string (vs un enum dedicado por kind) es aceptable
// porque la lista esta hardcodeada en buildPreflightChecks; cualquier
// drift se va a notar en los tests.
func resolveCheck(c PreflightCheck, p wizard.Pipeline, inputKind, inputValue string, cliInstalled map[string]bool, cliSkills map[string]map[string]bool) PreflightCheck {
	switch {
	case strings.HasPrefix(c.Label, "cli "):
		return resolveCliCheck(c, cliInstalled)
	case strings.HasPrefix(c.Label, "skill "):
		return resolveSkillCheck(c, cliSkills)
	case strings.HasPrefix(c.Label, "model "):
		return resolveModelCheck(c)
	case c.Label == "git repo context":
		info := repoctx.Detect()
		if info.InGitHubRepo {
			c.Status = PreflightOK
			c.Remedy = ""
			// Append del repo detectado al label para que el usuario sepa
			// contra cual repo va a resolver pr/issue (mismo patron que
			// "file readable: <path>").
			c.Label = "git repo context: " + info.Repo
			return c
		}
		c.Status = PreflightFail
		c.Remedy = "cd al repo correspondiente (gh repo view debe responder)"
		return c
	case c.Label == "gh auth status":
		if ghAuthFn() {
			c.Status = PreflightOK
			c.Remedy = ""
			return c
		}
		c.Status = PreflightFail
		c.Remedy = "gh auth login"
		return c
	case strings.HasPrefix(c.Label, "file readable:"):
		return resolveFileCheck(c, inputValue)
	case strings.HasPrefix(c.Label, "disk space"):
		return resolveDiskCheck(c)
	}
	// Label desconocido — tratarlo como warn para no falsear un OK
	// silencioso si alguien agrega un row sin handler.
	c.Status = PreflightWarn
	c.Remedy = "chequeo no implementado"
	return c
}

func resolveCliCheck(c PreflightCheck, cliInstalled map[string]bool) PreflightCheck {
	// Label format: "cli <name> instalado".
	parts := strings.Fields(c.Label)
	if len(parts) < 2 {
		c.Status = PreflightFail
		c.Remedy = "label cli malformado"
		return c
	}
	name := parts[1]
	// Preferimos consultar el cache de skills.Detect (que tambien chequea
	// LookPath) para mantener la sola fuente de verdad. Fallback a
	// lookPathFn por si el cache no tiene la entry (CLIs custom fuera del
	// set de 4 conocidos).
	if installed, ok := cliInstalled[name]; ok && installed {
		c.Status = PreflightOK
		c.Remedy = ""
		return c
	}
	if _, err := lookPathFn(name); err == nil {
		c.Status = PreflightOK
		c.Remedy = ""
		return c
	}
	c.Status = PreflightFail
	c.Remedy = fmt.Sprintf("instalar %s o cambiar el step a otro CLI", name)
	return c
}

// resolveModelCheck valida (cli, model) contra el whitelist hardcoded en
// models.go. Label format: "model <X> para cli <Y>". El preflight es el
// unico punto donde se chequea el whitelist; el runner asume que cualquier
// model que llegue a buildSpawnArgs ya pasó este filtro.
func resolveModelCheck(c PreflightCheck) PreflightCheck {
	// Label format: "model <model> para cli <cli>". Splitteamos por " para cli "
	// para tolerar nombres de modelo con espacios (defensive — el whitelist
	// actual no los tiene, pero el separador es univoco).
	const sep = " para cli "
	body := strings.TrimPrefix(c.Label, "model ")
	idx := strings.Index(body, sep)
	if idx < 0 {
		c.Status = PreflightFail
		c.Remedy = "label model malformado"
		return c
	}
	model := body[:idx]
	cli := body[idx+len(sep):]
	if err := ValidateModel(cli, model); err != nil {
		c.Status = PreflightFail
		c.Remedy = err.Error()
		return c
	}
	c.Status = PreflightOK
	c.Remedy = ""
	return c
}

func resolveSkillCheck(c PreflightCheck, cliSkills map[string]map[string]bool) PreflightCheck {
	// Label format: "skill <name> en <cli>".
	parts := strings.Fields(c.Label)
	if len(parts) != 4 {
		c.Status = PreflightFail
		c.Remedy = "label skill malformado"
		return c
	}
	name, cli := parts[1], parts[3]
	if set, ok := cliSkills[cli]; ok {
		if set[name] {
			c.Status = PreflightOK
			c.Remedy = ""
			return c
		}
	}
	c.Status = PreflightFail
	c.Remedy = fmt.Sprintf("instalar la skill %q en %s o editar el pipeline (cambiar a kind=prompt)", name, cli)
	return c
}

func resolveFileCheck(c PreflightCheck, value string) PreflightCheck {
	if value == "" {
		c.Status = PreflightFail
		c.Remedy = "input file vacio post-R1 (no deberia pasar)"
		return c
	}
	info, err := os.Stat(value)
	if err != nil {
		c.Status = PreflightFail
		c.Remedy = fmt.Sprintf("el archivo desaparecio entre R1 y R2: %s", value)
		return c
	}
	if info.IsDir() {
		c.Status = PreflightFail
		c.Remedy = fmt.Sprintf("ahora es un dir, no un file: %s", value)
		return c
	}
	c.Status = PreflightOK
	c.Remedy = ""
	return c
}

func resolveDiskCheck(c PreflightCheck) PreflightCheck {
	free := diskFreeFn(runDirForCheck())
	if free == 0 {
		c.Status = PreflightWarn
		c.Remedy = "no se pudo medir el espacio libre — continua bajo tu propio riesgo"
		return c
	}
	if free < minDiskBytes {
		c.Status = PreflightWarn
		c.Remedy = fmt.Sprintf("espacio libre bajo (%d MB); el run igual puede continuar", free/(1024*1024))
		return c
	}
	c.Status = PreflightOK
	c.Remedy = ""
	return c
}

// preflightVerdict resume la lista en una sola decision. Sirve para que el
// handler de teclas no recorra el slice cada keystroke.
type preflightVerdict int

const (
	preflightVerdictAllOK    preflightVerdict = iota // todos green: enter avanza
	preflightVerdictHasFail                          // algun rojo: enter bloqueado
	preflightVerdictOnlyWarn                         // verde + amarillos: enter pide confirm
)

func summarizePreflight(checks []PreflightCheck) preflightVerdict {
	hasWarn := false
	for _, c := range checks {
		switch c.Status {
		case PreflightFail:
			return preflightVerdictHasFail
		case PreflightWarn:
			hasWarn = true
		}
	}
	if hasWarn {
		return preflightVerdictOnlyWarn
	}
	return preflightVerdictAllOK
}

// enterPreflight construye + corre los chequeos y los guarda en m.Preflight.
// Tambien resetea preflightConfirm — un retry post-warning no debe arrastrar
// el "y ya confirme" del intento anterior.
func enterPreflight(m RunModel) RunModel {
	m.Screen = ScreenPreflight
	m.Preflight = buildPreflightChecks(m.Pipeline, m.Input.Kind, m.Input.Value)
	m.Preflight = runPreflightChecks(m.Preflight, m.Pipeline, m.Input.Kind, m.Input.Value)
	m.preflightConfirm = false
	return m
}

// updatePreflight maneja teclas de R2.
//
//   - enter: si hay fail → no-op (bloqueado).
//     si solo warns y aun no se confirmo → preflightConfirm = true (proximo
//     enter avanza). Si solo warns y ya se confirmo → avanza.
//     Si todo OK → avanza directo a la R3 placeholder (H4 implementa el real).
//   - r: limpia + re-corre los chequeos.
//   - d: salida total con un mensaje "corre `che doctor` y volve" (H3 no
//     suspende TUI; el TODO documenta el feature real para H4+).
//   - esc: vuelve al lister.
//   - ctrl+c: salida total.
func (m RunModel) updatePreflight(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		m.exitApp = true
		return m, tea.Quit
	case "esc":
		m.exitApp = false
		return m, tea.Quit
	case "r":
		m.Preflight = buildPreflightChecks(m.Pipeline, m.Input.Kind, m.Input.Value)
		m.Preflight = runPreflightChecks(m.Preflight, m.Pipeline, m.Input.Kind, m.Input.Value)
		m.preflightConfirm = false
		return m, nil
	case "d":
		// H3 todavia no suspende el TUI ni spawnea `che doctor` — lo
		// dejamos como exit total con flag exitApp=true; el caller
		// (cmd/root.go) puede surfacearlo en H4 o despues. Sin esto el
		// row "esc volver · r reintentar · d abrir doctor" del footer
		// quedaria muerto, por eso la tecla esta atada aunque la
		// implementacion sea minimal.
		m.exitApp = true
		return m, tea.Quit
	case "enter":
		switch summarizePreflight(m.Preflight) {
		case preflightVerdictHasFail:
			return m, nil
		case preflightVerdictOnlyWarn:
			if !m.preflightConfirm {
				m.preflightConfirm = true
				return m, nil
			}
			next, cmd := enterRunning(m)
			return next, cmd
		case preflightVerdictAllOK:
			next, cmd := enterRunning(m)
			return next, cmd
		}
	}
	return m, nil
}

// viewPreflight renderiza R2. La estructura sigue el mockup del doc: header
// breadcrumb (... · Preflight), lista con icono + label + (si aplica) linea de
// remedio indentada, footer con counts + hints. El nombre del pipeline ya
// vive en el segmento "Run · <name>" del breadcrumb — no lo repetimos en
// el ultimo segmento.
func (m RunModel) viewPreflight() string {
	var b strings.Builder
	crumb := append(runnerCrumb(m.Pipeline.Name), "Preflight")
	b.WriteString(breadcrumb(crumb...))
	b.WriteString("\n\n")

	for _, c := range m.Preflight {
		b.WriteString(renderPreflightRow(c))
		b.WriteString("\n")
	}

	// Counts + footer dinamicos segun el verdict.
	verdict := summarizePreflight(m.Preflight)
	fails, warns := 0, 0
	for _, c := range m.Preflight {
		switch c.Status {
		case PreflightFail:
			fails++
		case PreflightWarn:
			warns++
		}
	}

	b.WriteString("\n")
	switch verdict {
	case preflightVerdictHasFail:
		b.WriteString(errorStyle.Render(fmt.Sprintf("%d problema(s)", fails)))
		b.WriteString("\n")
		b.WriteString(hintStyle.Render("enter bloqueado · r reintentar · d abrir doctor · esc volver · ctrl+c salir"))
	case preflightVerdictOnlyWarn:
		if m.preflightConfirm {
			b.WriteString(warnStyle.Render(fmt.Sprintf("%d warning(s) — confirmar para continuar", warns)))
			b.WriteString("\n")
			b.WriteString(hintStyle.Render("enter continuar igual · r reintentar · esc volver · ctrl+c salir"))
		} else {
			b.WriteString(warnStyle.Render(fmt.Sprintf("%d warning(s)", warns)))
			b.WriteString("\n")
			b.WriteString(hintStyle.Render("enter para revisar antes de continuar · r reintentar · esc volver · ctrl+c salir"))
		}
	case preflightVerdictAllOK:
		b.WriteString(okStyle.Render("todo listo"))
		b.WriteString("\n")
		b.WriteString(hintStyle.Render("enter siguiente · esc volver · ctrl+c salir"))
	}
	b.WriteString("\n")
	return b.String()
}

// renderPreflightRow es el render de un row del checklist. Indentacion
// fija para que el remedio quede claramente subordinado al icono. Usar
// runa unicode (✓/✗/⏳/!) replica el mockup del doc.
func renderPreflightRow(c PreflightCheck) string {
	var icon, label string
	switch c.Status {
	case PreflightOK:
		icon = okStyle.Render("  ✓ ")
		label = c.Label
	case PreflightFail:
		icon = errorStyle.Render("  ✗ ")
		label = errorStyle.Render(c.Label)
	case PreflightWarn:
		icon = warnStyle.Render("  ! ")
		label = warnStyle.Render(c.Label)
	default:
		icon = dimStyle.Render("  ⏳ ")
		label = dimStyle.Render(c.Label)
	}
	if c.Remedy == "" {
		return icon + label
	}
	return icon + label + "\n" + dimStyle.Render("      remedio: "+c.Remedy)
}

// Estilos extra para preflight: warn = amarillo, ok = verde. errorStyle ya
// vive en runner.go (paleta dracula compartida con R1). Mantenerlos aca
// localiza el ambito de los estilos R2 — futuras screens no van a
// repintar estos colores por accidente.
var (
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6E3A1")).Bold(true)
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F9E2AF")).Bold(true)
)
