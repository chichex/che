package wizard

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/chichex/che/internal/runnermodels"
	"github.com/chichex/che/internal/skills"
)

// Approach: el toggle del validator (B2) cambia el set de campos visibles
// dentro de S2. Se resuelve con activeFocusOrder() — un slice "vivo" de
// targets de foco calculado a partir de stepEdit.validatorOn:
//
//   off → [Name, CLI, Kind, Content, Input, ValToggle]
//   on  → [Name, CLI, Kind, Content, Input, ValToggle, ValCLI, ValKind,
//          ValContent, ValMaxLoops, ValOnMaxLoops]
//
// stepNextFocus / stepPrevFocus operan sobre ese slice, por lo que tab/
// shift+tab "ven" exactamente lo que se renderiza. Apagar el toggle con
// foco en un campo del bloque redirige el foco a StepFocusValToggle (el
// ultimo target siempre visible). Defaults (mismo CLI que el step, kind=
// prompt, max_loops=3, on_max_loops=fail) se cargan en enterStepCreate
// para que el primer toggle on no encuentre el bloque vacio.

// enterStepCreate inicializa stepEdit para crear el step `idx` en mode=create
// y guarda sincronicamente status.stage=step. Carga la deteccion de skills
// si todavia no la tenemos, y elige el primer CLI installed como default —
// si no hay ninguno, el usuario igual ve la pantalla pero no puede guardar
// hasta instalar uno.
//
// TODO(autosave-async): el flow doc pide tambien debounce 800ms en cada
// field change. H3 entrega solo el save sincronico en transiciones (S1→S2
// + ctrl+s + esc); el debounce async se puede sumar despues sin tocar
// callers, capturando key events en updateStep y emitiendo tea.Tick.
func (m model) enterStepCreate(idx int) (model, tea.Cmd) {
	if !m.skillsCacheSet {
		m.skillsCache = skills.Detect("")
		m.skillsCacheSet = true
	}

	defaultCLI := stepCLIs[0]
	for _, name := range stepCLIs {
		if m.isInstalled(name) {
			defaultCLI = name
			break
		}
	}

	m.stepEdit = stepEditState{
		idx:          idx,
		mode:         "create",
		focus:        StepFocusName,
		nameInput:    newSingleLine("ej: collect-signals"),
		contentInput: newMultiLine("ej: extrae las metricas anomalas del payload"),
		cli:          defaultCLI,
		// model: default segun el CLI elegido (opencode → ""). Si el
		// usuario cambia el CLI, afterCLIChange resemilla este campo.
		model: runnermodels.DefaultModel(defaultCLI),
		kind:  KindPrompt,
		input: InputText,

		// Bloque validator (B2): off por default. Los defaults del flow
		// doc — mismo CLI que el step, kind=prompt, max_loops=3,
		// on_max_loops=fail — se siembran ya para que el primer toggle on
		// no muestre un bloque vacio. valContentInput es independiente del
		// del step (un prompt/skill distinto).
		validatorOn:     false,
		valCLI:          defaultCLI,
		valModel:        runnermodels.DefaultModel(defaultCLI),
		valKind:         KindPrompt,
		valContentInput: newMultiLine("ej: verifica que el output respete el formato pedido"),
		valMaxLoops:     3,
		valOnMaxLoops:   OnMaxLoopsFail,
	}
	if m.stepEdit.idx >= 1 {
		m.stepEdit.input = InputPreviousOutput
	}

	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageStep
	m.pipeline.Status.StepIdx = idx
	m.pipeline.Status.StepMode = "create"
	m.pipeline.Status.LastSavedAt = time.Now()

	if err := Save(m.path, m.pipeline); err != nil {
		m.stepEdit.errMsg = "no se pudo guardar: " + err.Error()
	}

	m.screen = ScreenStep
	return m, nil
}

// enterStepEdit inicializa stepEdit cargando el step ya existente en
// pipeline.Steps[idx], en mode=edit. Espejo de enterStepCreate, salvo que
// los buffers + selecciones discretas + bloque validator se siembran desde
// el Step persistido. La persistencia con stage=step, step_idx=idx,
// step_mode=edit la ejerce stepBack al volver desde step N+1 (H5) y mas
// adelante S3 cuando H7 entre con la edicion desde el resumen.
//
// Si idx esta fuera de rango (no deberia pasar — el caller controla la
// invocacion), caemos a enterStepCreate(idx) como fallback defensivo.
func (m model) enterStepEdit(idx int) (model, tea.Cmd) {
	if idx < 0 || idx >= len(m.pipeline.Steps) {
		return m.enterStepCreate(idx)
	}
	if !m.skillsCacheSet {
		m.skillsCache = skills.Detect("")
		m.skillsCacheSet = true
	}

	st := m.pipeline.Steps[idx]

	nameInput := newSingleLine("ej: collect-signals")
	nameInput.SetValue(st.Name)

	contentInput := newMultiLine("ej: extrae las metricas anomalas del payload")
	if st.Kind == KindPrompt {
		contentInput.SetValue(st.Content)
	}

	// Skill cursor: si kind=skill, ubicar el cursor del picker en la
	// posicion del skill guardado (si todavia existe en la lista). Si la
	// skill desaparecio (CLI desinstalada, skill borrada), arrancamos en 0
	// y dejamos que buildStep emita el error correspondiente al guardar.
	skillCursor := 0
	if st.Kind == KindSkill {
		for i, s := range m.skillsForCLI(st.CLI) {
			if s.Name == st.Content {
				skillCursor = i
				break
			}
		}
	}

	// Bloque validator. Si Step.Validator == nil, mantenemos los defaults
	// de "primer toggle on" (mismo CLI que el step, prompt, 3, fail) para
	// que prender el toggle muestre algo razonable; validatorOn arranca
	// false. Si Validator != nil, sembramos cli/kind/content/maxLoops/
	// onMaxLoops desde el bloque guardado.
	validatorOn := st.Validator != nil
	valCLI := st.CLI
	valKind := KindPrompt
	valContent := ""
	valMaxLoops := 3
	valOnMaxLoops := OnMaxLoopsFail
	valModel := runnermodels.DefaultModel(valCLI)
	if st.Validator != nil {
		valCLI = st.Validator.CLI
		valKind = st.Validator.Kind
		valContent = st.Validator.Content
		valModel = st.Validator.Model
		if valModel == "" {
			valModel = runnermodels.DefaultModel(valCLI)
		}
		if st.MaxLoops > 0 {
			valMaxLoops = st.MaxLoops
		}
		if st.OnMaxLoops != "" {
			valOnMaxLoops = st.OnMaxLoops
		}
	}

	// Modelo del step: el YAML puede traer vacio (default del CLI) o un
	// alias/nombre completo. Si esta vacio cargamos el default del CLI
	// para que la pill correspondiente aparezca seleccionada y que
	// guardar sin tocar nada conserve el comportamiento previo (no
	// escribimos `model:` en buildStep si no difiere del default).
	stepModel := st.Model
	if stepModel == "" {
		stepModel = runnermodels.DefaultModel(st.CLI)
	}

	valContentInput := newMultiLine("ej: verifica que el output respete el formato pedido")
	if valKind == KindPrompt {
		valContentInput.SetValue(valContent)
	}
	valSkillCursor := 0
	if valKind == KindSkill {
		for i, s := range m.skillsForCLI(valCLI) {
			if s.Name == valContent {
				valSkillCursor = i
				break
			}
		}
	}

	m.stepEdit = stepEditState{
		idx:             idx,
		mode:            "edit",
		focus:           StepFocusName,
		nameInput:       nameInput,
		contentInput:    contentInput,
		cli:             st.CLI,
		model:           stepModel,
		kind:            st.Kind,
		input:           st.Input,
		skillCursor:     skillCursor,
		validatorOn:     validatorOn,
		valCLI:          valCLI,
		valModel:        valModel,
		valKind:         valKind,
		valContentInput: valContentInput,
		valSkillCursor:  valSkillCursor,
		valMaxLoops:     valMaxLoops,
		valOnMaxLoops:   valOnMaxLoops,
	}

	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageStep
	m.pipeline.Status.StepIdx = idx
	m.pipeline.Status.StepMode = "edit"
	m.pipeline.Status.LastSavedAt = time.Now()

	if err := Save(m.path, m.pipeline); err != nil {
		m.stepEdit.errMsg = "no se pudo guardar: " + err.Error()
	}

	m.screen = ScreenStep
	return m, nil
}

// updateStep maneja teclas en S2.
func (m model) updateStep(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return m.openCancel(ScreenStep)
	case "esc":
		return m.stepBack()
	case "tab", "down":
		// down se comporta como tab salvo en filas que tienen su propia
		// semantica para arriba/abajo: los toggles (Kind del step, Kind
		// del validator y el toggle del propio validator) y los skill
		// pickers con items (navegan la lista).
		if key.String() == "down" && m.stepRowOwnsVerticalKey() {
			break
		}
		m.stepEdit.focus = m.stepNextFocus()
		return m, nil
	case "shift+tab", "up":
		if key.String() == "up" && m.stepRowOwnsVerticalKey() {
			break
		}
		m.stepEdit.focus = m.stepPrevFocus()
		return m, nil
	case "enter":
		// enter en el ultimo campo activo abre el modal "step listo —
		// agregar otro / finalizar / volver" para que la decision sea
		// explicita (sin esto el enter cerraba el wizard de pecho).
		// ctrl+s y ctrl+n siguen siendo atajos directos sin modal.
		if m.stepIsLastFocus() {
			return m.openSaveChoice()
		}
		m.stepEdit.focus = m.stepNextFocus()
		return m, nil
	case "ctrl+s":
		return m.stepSaveAndFinish()
	case "ctrl+n":
		return m.stepSaveAndAddAnother()
	}

	switch m.stepEdit.focus {
	case StepFocusName:
		return m.stepHandleNameKey(key)
	case StepFocusCLI:
		return m.stepHandleCLIKey(key)
	case StepFocusModel:
		return m.stepHandleModelKey(key)
	case StepFocusKind:
		return m.stepHandleKindKey(key)
	case StepFocusContent:
		return m.stepHandleContentKey(key)
	case StepFocusInput:
		return m.stepHandleInputKey(key)
	case StepFocusValToggle:
		return m.stepHandleValToggleKey(key)
	case StepFocusValCLI:
		return m.stepHandleValCLIKey(key)
	case StepFocusValModel:
		return m.stepHandleValModelKey(key)
	case StepFocusValKind:
		return m.stepHandleValKindKey(key)
	case StepFocusValContent:
		return m.stepHandleValContentKey(key)
	case StepFocusValMaxLoops:
		return m.stepHandleValMaxLoopsKey(key)
	case StepFocusValOnMaxLoops:
		return m.stepHandleValOnMaxLoopsKey(key)
	}
	return m, nil
}

func (m model) stepHandleNameKey(key tea.KeyMsg) (model, tea.Cmd) {
	// enter ya lo capturo updateStep como "siguiente foco" — aca solo
	// pasamos el resto al text input.
	if m.stepEdit.nameInput.HandleKey(key.String(), key.Runes) {
		m.stepEdit.errMsg = ""
	}
	return m, nil
}

// stepHandleCLIKey: left/right o digitos 1-4 cyclan entre las 4 pills.
// pills no instaladas igual aceptan seleccion para que el usuario sepa
// donde estan, pero ctrl+s la rechaza si la elegida no esta installed.
func (m model) stepHandleCLIKey(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.stepEdit.cli = stepNeighborCLI(m.stepEdit.cli, -1)
		m.afterCLIChange()
		return m, nil
	case "right", "l":
		m.stepEdit.cli = stepNeighborCLI(m.stepEdit.cli, +1)
		m.afterCLIChange()
		return m, nil
	case "1", "2", "3", "4":
		idx := int(key.String()[0] - '1')
		if idx >= 0 && idx < len(stepCLIs) {
			m.stepEdit.cli = stepCLIs[idx]
			m.afterCLIChange()
		}
		return m, nil
	}
	return m, nil
}

// afterCLIChange limpia el estado UI dependiente del CLI: resetea el
// cursor del picker y, si el kind era skill pero el CLI nuevo no tiene
// skills, revierte a prompt para no dejar la pantalla en un estado donde
// la toggle muestre algo que ya no se puede elegir.
//
// Tambien sincroniza valCLI con el step CLI mientras el validator siga
// off — coherente con "default conservador = mismo CLI" (B2 del flow
// doc). Una vez que el usuario prende el toggle, valCLI deja de seguir
// al step CLI para no pisar elecciones explicitas posteriores.
func (m *model) afterCLIChange() {
	m.stepEdit.skillCursor = 0
	m.stepEdit.errMsg = ""
	// Resemilla model al default del CLI nuevo — el alias del CLI viejo
	// (ej. "opus" para claude) muy probablemente no es valido para el CLI
	// nuevo (codex/gemini). Es mejor caer al default que dejar al usuario
	// con un modelo invalido y un mensaje del preflight despues.
	m.stepEdit.model = runnermodels.DefaultModel(m.stepEdit.cli)
	if m.stepEdit.kind == KindSkill && !m.cliHasSkills(m.stepEdit.cli) {
		m.stepEdit.kind = KindPrompt
	}
	if !m.stepEdit.validatorOn {
		m.stepEdit.valCLI = m.stepEdit.cli
		m.stepEdit.valModel = runnermodels.DefaultModel(m.stepEdit.valCLI)
		if m.stepEdit.valKind == KindSkill && !m.cliHasSkills(m.stepEdit.valCLI) {
			m.stepEdit.valKind = KindPrompt
		}
	}
}

// stepHandleModelKey cyclea entre los modelos validos para el CLI actual.
// Si el CLI no soporta override (opencode o uno desconocido), la tecla es
// no-op: la pill se renderiza "(no aplica)" y el campo permanece fijo en
// "" (que es lo que termina yendo al YAML como omitido).
func (m model) stepHandleModelKey(key tea.KeyMsg) (model, tea.Cmd) {
	opts := runnermodels.ModelsForCLI(m.stepEdit.cli)
	if len(opts) == 0 {
		return m, nil
	}
	switch key.String() {
	case "left", "h":
		m.stepEdit.model = stepNeighbor(opts, m.stepEdit.model, -1)
		m.stepEdit.errMsg = ""
		return m, nil
	case "right", "l":
		m.stepEdit.model = stepNeighbor(opts, m.stepEdit.model, +1)
		m.stepEdit.errMsg = ""
		return m, nil
	}
	if key.String() >= "1" && key.String() <= "9" {
		idx := int(key.String()[0] - '1')
		if idx >= 0 && idx < len(opts) {
			m.stepEdit.model = opts[idx]
			m.stepEdit.errMsg = ""
		}
	}
	return m, nil
}

// stepHandleValModelKey: equivalente para el bloque validator.
func (m model) stepHandleValModelKey(key tea.KeyMsg) (model, tea.Cmd) {
	opts := runnermodels.ModelsForCLI(m.stepEdit.valCLI)
	if len(opts) == 0 {
		return m, nil
	}
	switch key.String() {
	case "left", "h":
		m.stepEdit.valModel = stepNeighbor(opts, m.stepEdit.valModel, -1)
		m.stepEdit.errMsg = ""
		return m, nil
	case "right", "l":
		m.stepEdit.valModel = stepNeighbor(opts, m.stepEdit.valModel, +1)
		m.stepEdit.errMsg = ""
		return m, nil
	}
	if key.String() >= "1" && key.String() <= "9" {
		idx := int(key.String()[0] - '1')
		if idx >= 0 && idx < len(opts) {
			m.stepEdit.valModel = opts[idx]
			m.stepEdit.errMsg = ""
		}
	}
	return m, nil
}

func (m model) stepHandleKindKey(key tea.KeyMsg) (model, tea.Cmd) {
	hasSkills := m.cliHasSkills(m.stepEdit.cli)
	switch key.String() {
	case "left", "right", "h", "l", " ":
		// Toggle horizontal: solo ←/→/h/l/space alternan. ↑/↓ se reservan
		// al ciclo de foco vertical (updateStep) para no chocar con la
		// expectativa "↑ me lleva al campo de arriba". Si el CLI no tiene
		// skills, la toggle queda fija en prompt y la tecla es no-op —
		// coherente con que la pill "skill" ni siquiera se renderiza.
		if !hasSkills {
			return m, nil
		}
		if m.stepEdit.kind == KindPrompt {
			m.stepEdit.kind = KindSkill
		} else {
			m.stepEdit.kind = KindPrompt
		}
		m.stepEdit.skillCursor = 0
		m.stepEdit.errMsg = ""
		return m, nil
	case "p":
		m.stepEdit.kind = KindPrompt
		m.stepEdit.errMsg = ""
		return m, nil
	case "s":
		if !hasSkills {
			return m, nil
		}
		m.stepEdit.kind = KindSkill
		m.stepEdit.errMsg = ""
		return m, nil
	}
	return m, nil
}

// stepHandleContentKey deriva entre textarea (kind=prompt) y skill picker
// (kind=skill). En el picker up/down navegan; en textarea los caracteres
// se insertan literal.
func (m model) stepHandleContentKey(key tea.KeyMsg) (model, tea.Cmd) {
	if m.stepEdit.kind == KindSkill {
		list := m.skillsForCLI(m.stepEdit.cli)
		if len(list) == 0 {
			return m, nil
		}
		switch key.String() {
		case "up", "k":
			if m.stepEdit.skillCursor > 0 {
				m.stepEdit.skillCursor--
				m.stepEdit.errMsg = ""
				return m, nil
			}
			// ya estabamos en el primer item: salir del picker hacia
			// arriba (Kind) en vez de quedar trabado.
			m.stepEdit.focus = m.stepPrevFocus()
			m.stepEdit.errMsg = ""
			return m, nil
		case "down", "j":
			if m.stepEdit.skillCursor < len(list)-1 {
				m.stepEdit.skillCursor++
				m.stepEdit.errMsg = ""
				return m, nil
			}
			// ultimo item: salir hacia abajo (Input).
			m.stepEdit.focus = m.stepNextFocus()
			m.stepEdit.errMsg = ""
			return m, nil
		}
		return m, nil
	}

	// kind=prompt — tipear en el textarea multilinea.
	if m.stepEdit.contentInput.HandleKey(key.String(), key.Runes) {
		m.stepEdit.errMsg = ""
	}
	return m, nil
}

// stepHandleInputKey cyclea entre las opciones del enum input dependiente
// de la posicion del step (B1). El "mutex visual" (B1) se mantiene como
// senal — las 6 base se renderizan dim cuando previous_output esta activo
// para comunicar que esa es la eleccion dominante para steps encadenados —
// pero todas las opciones siguen siendo seleccionables. La interpretacion
// literal "ignora cambios" del spec encerraba al usuario en previous_output
// sin un atajo de salida (la unica via era editar el step desde S3, que
// todavia no existe).
//
// Excepcion: las opciones cubiertas por inputDisabled (hoy pr/issue cuando
// el cwd no esta dentro de un repo de github) se saltan en navegacion
// ←/→/h/l y se rechazan al hacer 1-9. Sin esto el cursor podria aterrizar
// en una pill que el runner no puede ofrecer (el preflight ya bloquea, pero
// es preferible que la senal aparezca temprano en el wizard).
func (m model) stepHandleInputKey(key tea.KeyMsg) (model, tea.Cmd) {
	options := inputsForStepIdx(m.stepEdit.idx)
	switch key.String() {
	case "left", "h":
		m.stepEdit.input = stepNeighborSkipDisabled(options, m.stepEdit.input, -1)
		m.stepEdit.errMsg = ""
		return m, nil
	case "right", "l":
		m.stepEdit.input = stepNeighborSkipDisabled(options, m.stepEdit.input, +1)
		m.stepEdit.errMsg = ""
		return m, nil
	}
	// digito 1..N: jump directo. Si la opcion esta disabled (pr/issue sin
	// repo) el jump es no-op — coherente con el cyclado que la salta.
	if key.String() >= "1" && key.String() <= "9" {
		idx := int(key.String()[0] - '1')
		if idx >= 0 && idx < len(options) && !inputDisabled(options[idx]) {
			m.stepEdit.input = options[idx]
			m.stepEdit.errMsg = ""
		}
	}
	return m, nil
}

// stepHandleValToggleKey alterna validatorOn con left/right/space/up/down/y/n.
// Apagar con foco previo en un campo del bloque validator es imposible
// aca (este handler corre solo si focus == StepFocusValToggle), pero al
// apagar conservamos el foco en el toggle — coherente con la idea de
// que el toggle es el "ancla" del bloque.
func (m model) stepHandleValToggleKey(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "left", "right", "h", "l", " ":
		// Toggle horizontal: ↑/↓ se reservan a foco vertical.
		m.stepEdit.validatorOn = !m.stepEdit.validatorOn
		m.stepEdit.errMsg = ""
		// si encendimos por primera vez y el CLI default fue cambiando
		// despues de enterStepCreate, valCLI queda con el seed viejo —
		// es aceptable (defaults son hints, no contratos).
		return m, nil
	case "y":
		m.stepEdit.validatorOn = true
		m.stepEdit.errMsg = ""
		return m, nil
	case "n":
		m.stepEdit.validatorOn = false
		m.stepEdit.errMsg = ""
		return m, nil
	}
	return m, nil
}

func (m model) stepHandleValCLIKey(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.stepEdit.valCLI = stepNeighborCLI(m.stepEdit.valCLI, -1)
		m.afterValCLIChange()
		return m, nil
	case "right", "l":
		m.stepEdit.valCLI = stepNeighborCLI(m.stepEdit.valCLI, +1)
		m.afterValCLIChange()
		return m, nil
	case "1", "2", "3", "4":
		idx := int(key.String()[0] - '1')
		if idx >= 0 && idx < len(stepCLIs) {
			m.stepEdit.valCLI = stepCLIs[idx]
			m.afterValCLIChange()
		}
		return m, nil
	}
	return m, nil
}

func (m *model) afterValCLIChange() {
	m.stepEdit.valSkillCursor = 0
	m.stepEdit.errMsg = ""
	m.stepEdit.valModel = runnermodels.DefaultModel(m.stepEdit.valCLI)
	if m.stepEdit.valKind == KindSkill && !m.cliHasSkills(m.stepEdit.valCLI) {
		m.stepEdit.valKind = KindPrompt
	}
}

func (m model) stepHandleValKindKey(key tea.KeyMsg) (model, tea.Cmd) {
	hasSkills := m.cliHasSkills(m.stepEdit.valCLI)
	switch key.String() {
	case "left", "right", "h", "l", " ":
		// Toggle horizontal: ↑/↓ se reservan a foco vertical.
		if !hasSkills {
			return m, nil
		}
		if m.stepEdit.valKind == KindPrompt {
			m.stepEdit.valKind = KindSkill
		} else {
			m.stepEdit.valKind = KindPrompt
		}
		m.stepEdit.valSkillCursor = 0
		m.stepEdit.errMsg = ""
		return m, nil
	case "p":
		m.stepEdit.valKind = KindPrompt
		m.stepEdit.errMsg = ""
		return m, nil
	case "s":
		if !hasSkills {
			return m, nil
		}
		m.stepEdit.valKind = KindSkill
		m.stepEdit.errMsg = ""
		return m, nil
	}
	return m, nil
}

func (m model) stepHandleValContentKey(key tea.KeyMsg) (model, tea.Cmd) {
	if m.stepEdit.valKind == KindSkill {
		list := m.skillsForCLI(m.stepEdit.valCLI)
		if len(list) == 0 {
			return m, nil
		}
		switch key.String() {
		case "up", "k":
			if m.stepEdit.valSkillCursor > 0 {
				m.stepEdit.valSkillCursor--
				m.stepEdit.errMsg = ""
				return m, nil
			}
			m.stepEdit.focus = m.stepPrevFocus()
			m.stepEdit.errMsg = ""
			return m, nil
		case "down", "j":
			if m.stepEdit.valSkillCursor < len(list)-1 {
				m.stepEdit.valSkillCursor++
				m.stepEdit.errMsg = ""
				return m, nil
			}
			m.stepEdit.focus = m.stepNextFocus()
			m.stepEdit.errMsg = ""
			return m, nil
		}
		return m, nil
	}

	// kind=prompt — textarea.
	if m.stepEdit.valContentInput.HandleKey(key.String(), key.Runes) {
		m.stepEdit.errMsg = ""
	}
	return m, nil
}

// stepHandleValMaxLoopsKey cyclea entre las pills 1..5 (default 3). Q
// cerrado en docs/manage-pipelines-flow.html — para valores fuera de
// rango, $EDITOR (H8).
func (m model) stepHandleValMaxLoopsKey(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.stepEdit.valMaxLoops = neighborInt(maxLoopsOptions, m.stepEdit.valMaxLoops, -1)
		m.stepEdit.errMsg = ""
		return m, nil
	case "right", "l":
		m.stepEdit.valMaxLoops = neighborInt(maxLoopsOptions, m.stepEdit.valMaxLoops, +1)
		m.stepEdit.errMsg = ""
		return m, nil
	}
	if key.String() >= "1" && key.String() <= "5" {
		m.stepEdit.valMaxLoops = int(key.String()[0] - '0')
		m.stepEdit.errMsg = ""
	}
	return m, nil
}

func (m model) stepHandleValOnMaxLoopsKey(key tea.KeyMsg) (model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.stepEdit.valOnMaxLoops = stepNeighbor(onMaxLoopsOptions, m.stepEdit.valOnMaxLoops, -1)
		m.stepEdit.errMsg = ""
		return m, nil
	case "right", "l":
		m.stepEdit.valOnMaxLoops = stepNeighbor(onMaxLoopsOptions, m.stepEdit.valOnMaxLoops, +1)
		m.stepEdit.errMsg = ""
		return m, nil
	}
	if key.String() >= "1" && key.String() <= "9" {
		idx := int(key.String()[0] - '1')
		if idx >= 0 && idx < len(onMaxLoopsOptions) {
			m.stepEdit.valOnMaxLoops = onMaxLoopsOptions[idx]
			m.stepEdit.errMsg = ""
		}
	}
	return m, nil
}

// stepBack: esc en S2.
//   - mode=create step 0: vuelve a S1, status.stage=info, descarta lo
//     tipeado del step (no se habia pusheado a pipeline.Steps todavia).
//   - mode=create step 1+: vuelve al step previo en mode=edit. El step
//     actual (mode=create) se descarta — sus buffers se reinician via
//     enterStepEdit; el step previo ya esta en pipeline.Steps porque
//     ctrl+n lo pusheo al avanzar.
//   - mode=edit: vuelve a S3 sin guardar los cambios del step. El modelo
//     en RAM mantiene el step original (no llamamos buildStep); solo
//     transicionamos al resumen.
func (m model) stepBack() (model, tea.Cmd) {
	if m.stepEdit.mode == "create" && m.stepEdit.idx == 0 {
		// Volver a S1 con status.stage=info, descartando el step tipeado.
		if m.pipeline.Status == nil {
			m.pipeline.Status = &Status{}
		}
		m.pipeline.Status.Stage = StageInfo
		m.pipeline.Status.StepIdx = 0
		m.pipeline.Status.StepMode = ""
		m.pipeline.Status.LastSavedAt = time.Now()
		// Limpiamos posibles steps fantasma para no dejar estado raro en
		// el archivo (defensive: en H3 esto siempre es vacio).
		m.pipeline.Steps = nil

		if err := Save(m.path, m.pipeline); err != nil {
			m.errMsg = "no se pudo guardar: " + err.Error()
		}
		m.screen = ScreenInfo
		return m, nil
	}
	if m.stepEdit.mode == "create" && m.stepEdit.idx >= 1 {
		// Step N+1 en mode=create se descarta: nunca llego a pushearse a
		// pipeline.Steps (eso lo hace ctrl+n / ctrl+s). Los buffers viven
		// solo en stepEdit, asi que enterStepEdit(idx-1) los pisa.
		return m.enterStepEdit(m.stepEdit.idx - 1)
	}
	// mode=edit: el step ya esta en pipeline.Steps con su contenido pre-
	// edicion (lo que viene de buffers en stepEdit aun no se materializo).
	// Volver al resumen sin tocar pipeline.Steps cumple "esc descarta los
	// cambios del step". Si llegamos aca con un pipeline vacio (no deberia
	// pasar — mode=edit implica algo en Steps) caemos a S1 como red.
	if len(m.pipeline.Steps) == 0 {
		m.screen = ScreenInfo
		return m, nil
	}
	return m.enterSummary()
}

// stepSaveAndAddAnother: ctrl+n en S2 mode=create. Si kind=prompt + content
// no vacio, primero dispara la review automatica con claude (modal
// ScreenStepReview). El skip de review (con o sin sugerencia aplicada)
// llama a stepSaveAndAddAnotherSkippingReview directamente.
func (m model) stepSaveAndAddAnother() (model, tea.Cmd) {
	if m.stepEdit.mode != "create" {
		return m, nil
	}
	if shouldRunPromptReview(m) {
		return m.startPromptReview("addanother")
	}
	return m.stepSaveAndAddAnotherSkippingReview()
}

// stepSaveAndAddAnotherSkippingReview es el path original (pre-review):
// build + save + enterStepCreate del siguiente. El modal de review llama
// aca despues de aplicar (o no) la sugerencia.
func (m model) stepSaveAndAddAnotherSkippingReview() (model, tea.Cmd) {
	if m.stepEdit.mode != "create" {
		return m, nil
	}
	step, err := m.buildStep()
	if err != nil {
		m.stepEdit.errMsg = err.Error()
		return m, nil
	}

	m.pipeline.Steps = append(m.pipeline.Steps, step)

	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageStep
	m.pipeline.Status.StepIdx = m.stepEdit.idx + 1
	m.pipeline.Status.StepMode = "create"
	m.pipeline.Status.LastSavedAt = time.Now()

	if err := Save(m.path, m.pipeline); err != nil {
		m.stepEdit.errMsg = "no se pudo guardar: " + err.Error()
		return m, nil
	}

	return m.enterStepCreate(m.stepEdit.idx + 1)
}

// stepSaveAndFinish: ctrl+s en S2 (y SaveFinish del modal de save choice).
// Si kind=prompt + content no vacio, dispara la review automatica con
// claude (modal ScreenStepReview). La aprobacion del modal (con o sin
// sugerencia aplicada) llama stepSaveAndFinishSkippingReview directamente.
func (m model) stepSaveAndFinish() (model, tea.Cmd) {
	if shouldRunPromptReview(m) {
		return m.startPromptReview("finish")
	}
	return m.stepSaveAndFinishSkippingReview()
}

// shouldRunPromptReview reporta si el step actual amerita pasar por la
// review automatica de prompt: kind=prompt, content no-blank, y todavia
// no estamos volviendo del modal (pendingSaveAction vacio). Si el usuario
// abrio el modal y ya decidio (guardar igual / aplicar sugerencia), saltea
// para no entrar en loop.
//
// CHE_DISABLE_PROMPT_REVIEW=1 desactiva el feature entero — lo usa el
// harness e2e (claude esta fakeado y no tiene matcher para la review;
// pedir uno por cada test seria ruido). En produccion no esta seteado.
func shouldRunPromptReview(m model) bool {
	if os.Getenv("CHE_DISABLE_PROMPT_REVIEW") == "1" {
		return false
	}
	if m.stepEdit.kind != KindPrompt {
		return false
	}
	if strings.TrimSpace(m.stepEdit.contentInput.Value()) == "" {
		return false
	}
	if m.stepEdit.pendingSaveAction != "" {
		return false
	}
	return true
}

// stepSaveAndFinishSkippingReview es el path original (pre-review): valida,
// pushea/reemplaza el step en pipeline.Steps, persiste con stage=step y
// transiciona a S3. El modal de review llama aca despues de aprobar.
func (m model) stepSaveAndFinishSkippingReview() (model, tea.Cmd) {
	step, err := m.buildStep()
	if err != nil {
		m.stepEdit.errMsg = err.Error()
		return m, nil
	}

	if m.stepEdit.mode == "edit" && m.stepEdit.idx < len(m.pipeline.Steps) {
		m.pipeline.Steps[m.stepEdit.idx] = step
	} else {
		m.pipeline.Steps = append(m.pipeline.Steps, step)
	}

	if m.pipeline.Status == nil {
		m.pipeline.Status = &Status{}
	}
	m.pipeline.Status.Stage = StageStep
	m.pipeline.Status.StepIdx = m.stepEdit.idx
	m.pipeline.Status.StepMode = m.stepEdit.mode
	m.pipeline.Status.LastSavedAt = time.Now()

	if err := Save(m.path, m.pipeline); err != nil {
		m.stepEdit.errMsg = "no se pudo guardar: " + err.Error()
		return m, nil
	}

	return m.enterSummary()
}

// buildStep arma el Step a partir del estado UI, con validaciones inline.
// Devuelve un error en castellano que se muestra como hint al usuario.
//
// validatorOn=false → Step.Validator nil + MaxLoops=0 (omitempty) +
// OnMaxLoops="" — el YAML queda sin las claves del bloque (max_loops
// implicito 1, segun el flow doc).
//
// validatorOn=true → Step.Validator con cli/kind/content + MaxLoops/
// OnMaxLoops materializados, defaults max_loops=3 y on_max_loops=fail.
func (m model) buildStep() (Step, error) {
	name := strings.TrimSpace(m.stepEdit.nameInput.Value())
	if name == "" {
		return Step{}, fmt.Errorf("el nombre del step no puede estar vacio")
	}
	// duplicate-name check dentro del pipeline (solo entre steps ya
	// pusheados — en H3 esto es siempre vacio, pero la red ya esta puesta).
	for i, s := range m.pipeline.Steps {
		if i == m.stepEdit.idx && m.stepEdit.mode == "edit" {
			continue
		}
		if s.Name == name {
			return Step{}, fmt.Errorf("ya hay otro step con nombre %q", name)
		}
	}

	cli := m.stepEdit.cli
	if cli == "" {
		return Step{}, fmt.Errorf("elegi un CLI para el step")
	}
	if !m.isInstalled(cli) {
		return Step{}, fmt.Errorf("%s no esta instalado — corre `che doctor` o elegi otro", cli)
	}

	kind := m.stepEdit.kind
	var content string
	switch kind {
	case KindPrompt:
		content = strings.TrimSpace(m.stepEdit.contentInput.Value())
		if content == "" {
			return Step{}, fmt.Errorf("escribi un prompt para el step (o cambia a kind=skill)")
		}
	case KindSkill:
		list := m.skillsForCLI(cli)
		if len(list) == 0 {
			return Step{}, fmt.Errorf("%s no tiene skills instaladas — cambia a kind=prompt", cli)
		}
		if m.stepEdit.skillCursor >= len(list) {
			return Step{}, fmt.Errorf("eleccion de skill invalida")
		}
		content = list[m.stepEdit.skillCursor].Name
	default:
		return Step{}, fmt.Errorf("kind invalido: %s", kind)
	}

	in := m.stepEdit.input
	if in == "" {
		return Step{}, fmt.Errorf("elegi un tipo de input para el step")
	}
	allowed := inputsForStepIdx(m.stepEdit.idx)
	if !contains(allowed, in) {
		return Step{}, fmt.Errorf("input %q no es valido para el step %d", in, m.stepEdit.idx+1)
	}
	// Coherencia con el render disabled: si el contexto no soporta pr/issue
	// (sin repo de github en el cwd), rechazamos el guardado con un hint
	// claro. El cyclado ya salta esas opciones, asi que llegar aca implica
	// que el wizard arranco con repo y se "perdio" antes del save (raro,
	// pero la red defensiva lo cubre).
	if inputDisabled(in) {
		return Step{}, fmt.Errorf("input %q requiere un repo de github en el cwd", in)
	}

	step := Step{
		Name:    name,
		CLI:     cli,
		Kind:    kind,
		Content: content,
		Input:   in,
	}

	// model: solo lo escribimos al YAML si difiere del default del CLI.
	// Mantener el YAML minimo es consistente con otros campos opcionales
	// (validator, max_loops, on_max_loops). El runner cae al mismo default
	// cuando el campo esta vacio, asi que el comportamiento no cambia.
	if chosen := m.stepEdit.model; chosen != "" && chosen != runnermodels.DefaultModel(cli) {
		step.Model = chosen
	}

	if m.stepEdit.validatorOn {
		v, maxLoops, onMaxLoops, err := m.buildValidator()
		if err != nil {
			return Step{}, err
		}
		step.Validator = v
		step.MaxLoops = maxLoops
		step.OnMaxLoops = onMaxLoops
	}

	return step, nil
}

// buildValidator arma el bloque Validator a partir del estado UI con las
// mismas reglas que buildStep para sus campos analogos. Devuelve max_loops
// y on_max_loops aparte porque viven en Step, no en Validator (el YAML
// los emite como hermanos del bloque, no anidados).
func (m model) buildValidator() (*Validator, int, string, error) {
	cli := m.stepEdit.valCLI
	if cli == "" {
		return nil, 0, "", fmt.Errorf("elegi un CLI para el validator")
	}
	if !m.isInstalled(cli) {
		return nil, 0, "", fmt.Errorf("validator: %s no esta instalado — corre `che doctor` o elegi otro", cli)
	}

	kind := m.stepEdit.valKind
	var content string
	switch kind {
	case KindPrompt:
		content = strings.TrimSpace(m.stepEdit.valContentInput.Value())
		if content == "" {
			return nil, 0, "", fmt.Errorf("escribi un prompt para el validator (o cambia kind=skill)")
		}
	case KindSkill:
		list := m.skillsForCLI(cli)
		if len(list) == 0 {
			return nil, 0, "", fmt.Errorf("validator: %s no tiene skills instaladas — cambia a kind=prompt", cli)
		}
		if m.stepEdit.valSkillCursor >= len(list) {
			return nil, 0, "", fmt.Errorf("validator: eleccion de skill invalida")
		}
		content = list[m.stepEdit.valSkillCursor].Name
	default:
		return nil, 0, "", fmt.Errorf("validator: kind invalido: %s", kind)
	}

	maxLoops := m.stepEdit.valMaxLoops
	if maxLoops <= 0 {
		maxLoops = 3
	}

	onMax := m.stepEdit.valOnMaxLoops
	if !contains(onMaxLoopsOptions, onMax) {
		return nil, 0, "", fmt.Errorf("validator: on_max_loops invalido: %s", onMax)
	}

	v := &Validator{CLI: cli, Kind: kind, Content: content}
	// Misma regla que buildStep: model: se persiste solo si difiere del
	// default del CLI del validator.
	if chosen := m.stepEdit.valModel; chosen != "" && chosen != runnermodels.DefaultModel(cli) {
		v.Model = chosen
	}
	return v, maxLoops, onMax, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// stepInPicker reporta true sii el foco esta en content (del step) con
// kind=skill y hay al menos un item en la lista del picker. Bajo esa
// condicion up/down se reservan al picker; en cualquier otro caso
// (incluido picker vacio) up/down deben mover foco para no dejar al
// usuario trulado.
func (m *model) stepInPicker() bool {
	if m.stepEdit.focus != StepFocusContent {
		return false
	}
	if m.stepEdit.kind != KindSkill {
		return false
	}
	return m.cliHasSkills(m.stepEdit.cli)
}

// stepInValPicker es el equivalente para el bloque validator: foco en
// ValContent con kind=skill y skills disponibles para el CLI elegido.
func (m *model) stepInValPicker() bool {
	if m.stepEdit.focus != StepFocusValContent {
		return false
	}
	if m.stepEdit.valKind != KindSkill {
		return false
	}
	return m.cliHasSkills(m.stepEdit.valCLI)
}

// stepRowOwnsVerticalKey reporta si la fila bajo el foco actual quiere
// quedarse las teclas up/down. updateStep usa esto para decidir si
// up/down se traduce a "mover foco" o se delega al handler de la fila.
//
// Solo las filas con semantica vertical NATIVA (skill picker = lista
// scrolleable) consumen up/down. Los toggles horizontales (Kind / Validator
// si-no / ValKind) ya NO consumen up/down — la expectativa del usuario es
// que ↑ lo lleve al campo previo, no que alterne dentro de la fila.
// Para alternar el toggle quedan ←/→/space/(p/s/y/n shortcuts).
func (m *model) stepRowOwnsVerticalKey() bool {
	switch m.stepEdit.focus {
	case StepFocusContent:
		return m.stepInPicker()
	case StepFocusValContent:
		return m.stepInValPicker()
	}
	return false
}

// cliHasSkills reporta si hay al menos una skill registrada para el CLI
// dado. La toggle Kind oculta la opcion "skill" cuando esto es false —
// no tiene sentido ofrecer un kind que no se puede llenar.
func (m *model) cliHasSkills(name string) bool {
	return len(m.skillsForCLI(name)) > 0
}

// activeFocusOrder es la lista "viva" de targets de foco para el ciclo
// tab/shift+tab. Refleja exactamente lo que se renderiza: el bloque
// validator entra solo cuando validatorOn=true. Mantener este orden en
// un solo lugar evita que la UI muestre un campo que el ciclo no toca o
// viceversa.
func (m *model) activeFocusOrder() []StepFieldFocus {
	base := []StepFieldFocus{
		StepFocusName,
		StepFocusCLI,
		StepFocusModel,
		StepFocusKind,
		StepFocusContent,
		StepFocusInput,
		StepFocusValToggle,
	}
	if m.stepEdit.validatorOn {
		base = append(base,
			StepFocusValCLI,
			StepFocusValModel,
			StepFocusValKind,
			StepFocusValContent,
			StepFocusValMaxLoops,
			StepFocusValOnMaxLoops,
		)
	}
	return base
}

// indexInOrder devuelve la posicion del foco actual en activeFocusOrder.
// Si por algun motivo el foco quedo en un target inactivo (deberia ser
// imposible: stepHandleValToggleKey resetea), retornamos 0 — caer en el
// primer item es preferible a dejar al usuario sin avance.
func (m *model) indexInOrder(order []StepFieldFocus) int {
	for i, f := range order {
		if f == m.stepEdit.focus {
			return i
		}
	}
	return 0
}

func (m *model) stepNextFocus() StepFieldFocus {
	order := m.activeFocusOrder()
	idx := m.indexInOrder(order)
	return order[(idx+1)%len(order)]
}

func (m *model) stepPrevFocus() StepFieldFocus {
	order := m.activeFocusOrder()
	idx := m.indexInOrder(order)
	n := len(order)
	return order[(idx-1+n)%n]
}

// stepIsLastFocus reporta si el foco esta en el ultimo target del ciclo
// activo. enter sobre el ultimo target dispara "guardar y avanzar",
// equivalente a ctrl+s — coherente con la heuristica original de H3
// que usaba StepFocusInput como "ultimo campo".
func (m *model) stepIsLastFocus() bool {
	order := m.activeFocusOrder()
	if len(order) == 0 {
		return false
	}
	return m.stepEdit.focus == order[len(order)-1]
}

// stepNeighborSkipDisabled es como stepNeighbor pero salta las opciones
// reportadas por inputDisabled. Si todas las opciones estan disabled (caso
// patologico — text/file/url/none nunca lo estan), devuelve current sin
// tocar para no entrar en loop.
func stepNeighborSkipDisabled(options []string, current string, delta int) string {
	if len(options) == 0 {
		return current
	}
	idx := -1
	for i, o := range options {
		if o == current {
			idx = i
			break
		}
	}
	if idx == -1 {
		// current desaparecio de la lista; arrancar en el primer enabled.
		for i, o := range options {
			if !inputDisabled(o) {
				return options[i]
			}
		}
		return options[0]
	}
	n := len(options)
	for step := 1; step <= n; step++ {
		next := options[((idx+delta*step)%n+n)%n]
		if !inputDisabled(next) {
			return next
		}
	}
	// Todas disabled — no cambiar.
	return current
}

// stepNeighbor devuelve el elemento adyacente (con wrap) en una lista.
func stepNeighbor(options []string, current string, delta int) string {
	if len(options) == 0 {
		return current
	}
	idx := -1
	for i, o := range options {
		if o == current {
			idx = i
			break
		}
	}
	if idx == -1 {
		return options[0]
	}
	n := len(options)
	return options[((idx+delta)%n+n)%n]
}

func stepNeighborCLI(current string, delta int) string {
	return stepNeighbor(stepCLIs, current, delta)
}

// neighborInt es la version []int de stepNeighbor, para max_loops.
func neighborInt(options []int, current, delta int) int {
	if len(options) == 0 {
		return current
	}
	idx := -1
	for i, o := range options {
		if o == current {
			idx = i
			break
		}
	}
	if idx == -1 {
		return options[0]
	}
	n := len(options)
	return options[((idx+delta)%n+n)%n]
}

// viewStep renderiza S2.
func (m model) viewStep() string {
	var b strings.Builder
	last := fmt.Sprintf("paso 2/3 · step %d (%s)", m.stepEdit.idx+1, m.stepEdit.mode)
	b.WriteString(breadcrumb("Create pipeline", last))
	b.WriteString("\n")
	if name := m.pipeline.Name; name != "" {
		// Linea de contexto: "Pipeline: <name>" dimmed para que el usuario
		// no se olvide de cual pipeline esta editando si tiene varios drafts.
		b.WriteString(dimStyle.Render("Pipeline: " + name))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(renderStepBreadcrumb(m))
	b.WriteString("\n")

	// nombre
	b.WriteString(renderLabeledField("Nombre del step", m.stepEdit.nameInput, m.stepEdit.focus == StepFocusName, m.width))
	b.WriteString("\n")

	// cli pills
	b.WriteString(labelStyle.Render("CLI"))
	if m.stepEdit.focus == StepFocusCLI {
		b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar · 1-4 jump"))
	}
	b.WriteString("\n")
	b.WriteString(renderCLIPills(m, m.stepEdit.focus == StepFocusCLI))
	b.WriteString("\n")

	// model pills (opcional segun CLI). opencode y CLIs desconocidos no
	// aceptan override — la fila igual aparece para que el layout no salte.
	b.WriteString(labelStyle.Render("Modelo"))
	if m.stepEdit.focus == StepFocusModel {
		if runnermodels.SupportsModelOverride(m.stepEdit.cli) {
			b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar · 1-9 jump"))
		} else {
			b.WriteString(dimStyle.Render("  ← foco · " + m.stepEdit.cli + " no soporta elegir modelo desde el YAML"))
		}
	}
	b.WriteString("\n")
	b.WriteString(renderModelPills(m.stepEdit.cli, m.stepEdit.model, m.stepEdit.focus == StepFocusModel))
	b.WriteString("\n")

	// kind toggle. Si el CLI elegido no tiene skills, ocultamos la pill
	// "skill" — y el hint cambia para que el usuario sepa por que no se
	// puede alternar.
	hasSkills := m.cliHasSkills(m.stepEdit.cli)
	b.WriteString(labelStyle.Render("Kind"))
	if m.stepEdit.focus == StepFocusKind {
		if hasSkills {
			b.WriteString(dimStyle.Render("  ← foco · ←/→ alternar · p/s atajo · tab/↑/↓ para salir"))
		} else {
			b.WriteString(dimStyle.Render("  ← foco · " + m.stepEdit.cli + " no tiene skills"))
		}
	}
	b.WriteString("\n")
	b.WriteString(renderKindToggle(m.stepEdit.kind, m.stepEdit.focus == StepFocusKind, hasSkills))
	b.WriteString("\n\n")

	// content
	b.WriteString(renderContent(m))
	b.WriteString("\n\n")

	// input enum
	b.WriteString(labelStyle.Render("Input"))
	if m.stepEdit.focus == StepFocusInput {
		b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar · 1-7 jump"))
	}
	b.WriteString("\n")
	b.WriteString(renderInputPills(m))
	b.WriteString("\n\n")

	// validator toggle (siempre visible) + bloque (B2: solo si on).
	b.WriteString(labelStyle.Render("¿Validar este step?"))
	if m.stepEdit.focus == StepFocusValToggle {
		b.WriteString(dimStyle.Render("  ← foco · ←/→/space alternar · y/n atajo"))
	}
	b.WriteString("\n")
	b.WriteString(renderValToggle(m.stepEdit.validatorOn, m.stepEdit.focus == StepFocusValToggle))
	b.WriteString("\n")

	if m.stepEdit.validatorOn {
		b.WriteString("\n")
		// El border-left + padding son la senal visual de "sub-bloque
		// opcional"; sin esto los labels (CLI/Kind/Prompt) se confunden
		// con los del step principal.
		b.WriteString(validatorBoxStyle.Render(renderValidatorBlock(m)))
		b.WriteString("\n")
	}

	if m.stepEdit.errMsg != "" {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("✗ " + m.stepEdit.errMsg))
		b.WriteString("\n")
	}

	if m.path != "" {
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("draft: " + m.path))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	// Hint del bottom: ctrl+n aparece solo en mode=create (mode=edit no
	// expone "+ agregar step"; eso lo hace el "+" desde S3 en H7).
	if m.stepEdit.mode == "create" {
		b.WriteString(hintStyle.Render("enter/tab/↓ siguiente · shift+tab/↑ anterior · ctrl+s guardar step · ctrl+n agregar otro step · esc volver"))
	} else {
		b.WriteString(hintStyle.Render("enter/tab/↓ siguiente · shift+tab/↑ anterior · ctrl+s guardar step · esc volver"))
	}
	b.WriteString("\n")
	return b.String()
}

// renderStepBreadcrumb dibuja una lista vertical de los steps del
// pipeline con el step actual resaltado. Sin esto el usuario que hizo
// ctrl+n no tiene contexto de "que step es este, cuales son los previos".
//
// Por step se rinde: numero + nombre + resumen "<cli> · <kind|skill:N> ·
// <input>" + chip "+ val:<cli>" si tiene validator. El nombre de skill
// y la presencia del validator son senales claves para entender el
// pipeline de un vistazo (cross-review vs self-review, que skill corre
// cada step).
//
// El step actual:
//   - mode=create + idx == len(Steps): rinde "(creando)" al final
//     (todavia no esta en pipeline.Steps).
//   - mode=edit:                       rinde el step de Steps[idx] con
//     marcador "▸ N. <name>  (editando)".
func renderStepBreadcrumb(m model) string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Steps:"))
	b.WriteString("\n")

	curIdx := m.stepEdit.idx
	creating := m.stepEdit.mode == "create"

	for i, s := range m.pipeline.Steps {
		marker := "  "
		isCurrent := i == curIdx && !creating
		if isCurrent {
			marker = selectedItem.Render("▸ ")
		}

		summary := stepSummary(s)
		var row string
		if isCurrent {
			row = selectedItem.Render(fmt.Sprintf("%d. %-22s", i+1, s.Name)) + "   " + dimStyle.Render(summary) + "  " + dimStyle.Italic(true).Render("(editando)")
		} else {
			row = fmt.Sprintf("%d. %-22s   %s", i+1, s.Name, dimStyle.Render(summary))
		}
		b.WriteString(marker + row + "\n")
	}
	if creating {
		marker := selectedItem.Render("▸ ")
		row := selectedItem.Render(fmt.Sprintf("%d. (creando)", curIdx+1))
		b.WriteString(marker + row + "\n")
	}
	return b.String()
}

// stepSummary devuelve la linea compacta "<cli> · <kind|skill:N> · <input>
// [+ val:<cli>]" para un Step persistido. Se usa en el breadcrumb para que
// el usuario vea de un vistazo: que CLI corre el step, si es prompt o
// skill (con el nombre cuando es skill), que input consume y si tiene
// validator (con el CLI del validator, util para distinguir cross-review
// de self-review).
func stepSummary(s Step) string {
	kindPart := s.Kind
	if s.Kind == KindSkill && s.Content != "" {
		kindPart = "skill:" + s.Content
	}
	out := s.CLI + " · " + kindPart + " · " + s.Input
	if s.Validator != nil {
		out += "  + val:" + s.Validator.CLI
	}
	return out
}

func renderCLIPills(m model, focused bool) string {
	var parts []string
	for _, name := range stepCLIs {
		installed := m.isInstalled(name)
		label := name
		if !installed {
			label = name + " ·off"
		}
		switch {
		case name == m.stepEdit.cli && focused:
			parts = append(parts, selectedItem.Render("["+label+"]"))
		case name == m.stepEdit.cli:
			parts = append(parts, selectedOff.Render("["+label+"]"))
		case !installed:
			parts = append(parts, dimStyle.Render(" "+label+" "))
		default:
			parts = append(parts, mutedItem.Render(" "+label+" "))
		}
	}
	return "  " + strings.Join(parts, "  ")
}

func renderKindToggle(kind string, focused, hasSkills bool) string {
	render := func(label string, on bool) string {
		switch {
		case on && focused:
			return selectedItem.Render("[" + label + "]")
		case on:
			return selectedOff.Render("[" + label + "]")
		default:
			return dimStyle.Render(" " + label + " ")
		}
	}
	out := "  " + render("prompt", kind == KindPrompt)
	if hasSkills {
		out += "  " + render("skill", kind == KindSkill)
	}
	return out
}

func renderContent(m model) string {
	focused := m.stepEdit.focus == StepFocusContent
	if m.stepEdit.kind == KindSkill {
		var b strings.Builder
		b.WriteString(labelStyle.Render("Skill"))
		if focused {
			b.WriteString(dimStyle.Render("  ← foco · ↑/↓ navegar"))
		}
		b.WriteString("\n")
		list := m.skillsForCLI(m.stepEdit.cli)
		if len(list) == 0 {
			b.WriteString(dimStyle.Render("  (sin skills para " + m.stepEdit.cli + " — cambia a kind=prompt)\n"))
			return b.String()
		}
		max := 6
		start := 0
		if m.stepEdit.skillCursor >= max {
			start = m.stepEdit.skillCursor - (max - 1)
		}
		end := start + max
		if end > len(list) {
			end = len(list)
		}
		for i := start; i < end; i++ {
			s := list[i]
			line := fmt.Sprintf("%-26s  %s", s.Name, dimStyle.Render(string(s.Scope)))
			if i == m.stepEdit.skillCursor {
				b.WriteString(selectedItem.Render("> ") + line + "\n")
			} else {
				b.WriteString("  " + line + "\n")
			}
		}
		return b.String()
	}

	style := inputBoxBorder
	if focused {
		style = inputBoxBorderFocus
	}
	label := labelStyle.Render("Prompt")
	if focused {
		label += dimStyle.Render("  ← foco · shift+enter / alt+enter para newline")
	}
	body := m.stepEdit.contentInput.view(focused)
	if inner := ContentInnerWidth(m.width); inner > 0 {
		body = WrapText(body, inner)
		style = style.Width(inner)
	}
	return label + "\n" + style.Render(body)
}

// ContentInnerWidth devuelve el ancho disponible (en columnas) dentro de
// la caja de input — ya descontados border (2) + padding (2) + un margen
// de seguridad. Devuelve 0 cuando todavia no recibimos un WindowSizeMsg
// (signal "no wrappear, dejar al terminal").
func ContentInnerWidth(termWidth int) int {
	if termWidth <= 0 {
		return 0
	}
	inner := termWidth - 6
	if inner < 20 {
		return 0
	}
	return inner
}

func renderInputPills(m model) string {
	options := inputsForStepIdx(m.stepEdit.idx)
	focused := m.stepEdit.focus == StepFocusInput
	var parts []string
	for _, o := range options {
		// pr/issue sin repo activo se rinden con sufijo "·off" (mismo patron
		// visual que un CLI no instalado en S2). dim + off comunica "esta
		// opcion existe pero el contexto no la habilita".
		if inputDisabled(o) {
			parts = append(parts, dimStyle.Render(" "+o+" ·off "))
			continue
		}
		switch {
		case o == m.stepEdit.input && focused:
			parts = append(parts, selectedItem.Render("["+o+"]"))
		case o == m.stepEdit.input:
			parts = append(parts, selectedOff.Render("["+o+"]"))
		default:
			parts = append(parts, dimStyle.Render(" "+o+" "))
		}
	}
	return "  " + strings.Join(parts, "  ")
}

func renderValToggle(on, focused bool) string {
	render := func(label string, active bool) string {
		switch {
		case active && focused:
			return selectedItem.Render("[" + label + "]")
		case active:
			return selectedOff.Render("[" + label + "]")
		default:
			return dimStyle.Render(" " + label + " ")
		}
	}
	return "  " + render("sí", on) + "  " + render("no", !on)
}

// renderValidatorBlock renderiza los 5 campos del bloque validator. El
// caller envuelve el resultado en validatorBoxStyle (border-left +
// padding) — eso ya sirve de "esto es un sub-bloque del step opcional",
// asi que los labels NO repiten el prefix "Validator · " en cada fila
// (seria ruido visual sobre la senal del border).
func renderValidatorBlock(m model) string {
	var b strings.Builder

	// Header dim al tope: deja explicito que arrancamos un sub-bloque.
	b.WriteString(dimStyle.Italic(true).Render("Bloque validator · cross-review en loop antes de avanzar"))
	b.WriteString("\n\n")

	// CLI pills
	b.WriteString(labelStyle.Render("CLI"))
	if m.stepEdit.focus == StepFocusValCLI {
		b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar · 1-4 jump"))
	}
	b.WriteString("\n")
	b.WriteString(renderValCLIPills(m))
	b.WriteString("\n")

	// Modelo del validator (mismo contrato que el del step).
	b.WriteString(labelStyle.Render("Modelo"))
	if m.stepEdit.focus == StepFocusValModel {
		if runnermodels.SupportsModelOverride(m.stepEdit.valCLI) {
			b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar · 1-9 jump"))
		} else {
			b.WriteString(dimStyle.Render("  ← foco · " + m.stepEdit.valCLI + " no soporta elegir modelo desde el YAML"))
		}
	}
	b.WriteString("\n")
	b.WriteString(renderModelPills(m.stepEdit.valCLI, m.stepEdit.valModel, m.stepEdit.focus == StepFocusValModel))
	b.WriteString("\n")

	// Kind toggle (solo prompt si el CLI no tiene skills).
	hasSkills := m.cliHasSkills(m.stepEdit.valCLI)
	b.WriteString(labelStyle.Render("Kind"))
	if m.stepEdit.focus == StepFocusValKind {
		if hasSkills {
			b.WriteString(dimStyle.Render("  ← foco · ←/→ alternar · p/s atajo"))
		} else {
			b.WriteString(dimStyle.Render("  ← foco · " + m.stepEdit.valCLI + " no tiene skills"))
		}
	}
	b.WriteString("\n")
	b.WriteString(renderKindToggle(m.stepEdit.valKind, m.stepEdit.focus == StepFocusValKind, hasSkills))
	b.WriteString("\n\n")

	// Content (textarea o picker).
	b.WriteString(renderValContent(m))
	b.WriteString("\n\n")

	// max_loops pills
	b.WriteString(labelStyle.Render("max_loops"))
	if m.stepEdit.focus == StepFocusValMaxLoops {
		b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar · 1-5 jump"))
	}
	b.WriteString("\n")
	b.WriteString(renderMaxLoopsPills(m))
	b.WriteString("\n")

	// on_max_loops pills
	b.WriteString(labelStyle.Render("on_max_loops"))
	if m.stepEdit.focus == StepFocusValOnMaxLoops {
		b.WriteString(dimStyle.Render("  ← foco · ←/→ cambiar"))
	}
	b.WriteString("\n")
	b.WriteString(renderOnMaxLoopsPills(m))
	return b.String()
}

func renderValCLIPills(m model) string {
	focused := m.stepEdit.focus == StepFocusValCLI
	var parts []string
	for _, name := range stepCLIs {
		installed := m.isInstalled(name)
		label := name
		if !installed {
			label = name + " ·off"
		}
		switch {
		case name == m.stepEdit.valCLI && focused:
			parts = append(parts, selectedItem.Render("["+label+"]"))
		case name == m.stepEdit.valCLI:
			parts = append(parts, selectedOff.Render("["+label+"]"))
		case !installed:
			parts = append(parts, dimStyle.Render(" "+label+" "))
		default:
			parts = append(parts, mutedItem.Render(" "+label+" "))
		}
	}
	return "  " + strings.Join(parts, "  ")
}

func renderValContent(m model) string {
	focused := m.stepEdit.focus == StepFocusValContent
	if m.stepEdit.valKind == KindSkill {
		var b strings.Builder
		b.WriteString(labelStyle.Render("Skill"))
		if focused {
			b.WriteString(dimStyle.Render("  ← foco · ↑/↓ navegar"))
		}
		b.WriteString("\n")
		list := m.skillsForCLI(m.stepEdit.valCLI)
		if len(list) == 0 {
			b.WriteString(dimStyle.Render("  (sin skills para " + m.stepEdit.valCLI + " — cambia a kind=prompt)\n"))
			return b.String()
		}
		max := 6
		start := 0
		if m.stepEdit.valSkillCursor >= max {
			start = m.stepEdit.valSkillCursor - (max - 1)
		}
		end := start + max
		if end > len(list) {
			end = len(list)
		}
		for i := start; i < end; i++ {
			s := list[i]
			line := fmt.Sprintf("%-26s  %s", s.Name, dimStyle.Render(string(s.Scope)))
			if i == m.stepEdit.valSkillCursor {
				b.WriteString(selectedItem.Render("> ") + line + "\n")
			} else {
				b.WriteString("  " + line + "\n")
			}
		}
		return b.String()
	}

	style := inputBoxBorder
	if focused {
		style = inputBoxBorderFocus
	}
	label := labelStyle.Render("Prompt")
	if focused {
		label += dimStyle.Render("  ← foco · shift+enter / alt+enter para newline")
	}
	body := m.stepEdit.valContentInput.view(focused)
	if inner := ContentInnerWidth(m.width); inner > 0 {
		body = WrapText(body, inner)
		style = style.Width(inner)
	}
	return label + "\n" + style.Render(body)
}

// renderModelPills renderiza la fila de modelos para el CLI dado. Sirve
// tanto al step principal como al validator — el caller pasa cli + el
// modelo seleccionado actual + si la fila esta focuseada.
//
// Si el CLI no esta en el whitelist (opencode o un CLI custom), en lugar
// de pills mostramos un texto dimmed "(no aplica — <cli> usa la config
// del propio CLI)". El cursor puede aterrizar en la fila igual; ←/→ son
// no-op para que el usuario entienda que es un campo informativo.
func renderModelPills(cli, selected string, focused bool) string {
	opts := runnermodels.ModelsForCLI(cli)
	if len(opts) == 0 {
		return "  " + dimStyle.Render("(no aplica — "+cli+" usa la config del propio CLI)")
	}
	var parts []string
	for _, m := range opts {
		switch {
		case m == selected && focused:
			parts = append(parts, selectedItem.Render("["+m+"]"))
		case m == selected:
			parts = append(parts, selectedOff.Render("["+m+"]"))
		default:
			parts = append(parts, dimStyle.Render(" "+m+" "))
		}
	}
	return "  " + strings.Join(parts, "  ")
}

func renderMaxLoopsPills(m model) string {
	focused := m.stepEdit.focus == StepFocusValMaxLoops
	var parts []string
	for _, n := range maxLoopsOptions {
		label := fmt.Sprintf("%d", n)
		switch {
		case n == m.stepEdit.valMaxLoops && focused:
			parts = append(parts, selectedItem.Render("["+label+"]"))
		case n == m.stepEdit.valMaxLoops:
			parts = append(parts, selectedOff.Render("["+label+"]"))
		default:
			parts = append(parts, dimStyle.Render(" "+label+" "))
		}
	}
	return "  " + strings.Join(parts, "  ")
}

func renderOnMaxLoopsPills(m model) string {
	focused := m.stepEdit.focus == StepFocusValOnMaxLoops
	var parts []string
	for _, o := range onMaxLoopsOptions {
		switch {
		case o == m.stepEdit.valOnMaxLoops && focused:
			parts = append(parts, selectedItem.Render("["+o+"]"))
		case o == m.stepEdit.valOnMaxLoops:
			parts = append(parts, selectedOff.Render("["+o+"]"))
		default:
			parts = append(parts, dimStyle.Render(" "+o+" "))
		}
	}
	return "  " + strings.Join(parts, "  ")
}
