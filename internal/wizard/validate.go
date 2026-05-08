package wizard

import (
	"fmt"

	"github.com/chichex/che/internal/skills"
)

// detectInstalledCLIs devuelve los CLIs detectados como instalados ahora
// mismo. Variable a nivel paquete para que los tests puedan inyectar una
// version mockeada sin tocar PATH ni levantar el harness completo. La
// version default usa skills.Detect("") (igual que el wizard) para que el
// runtime real comparta exactamente la misma deteccion.
var detectInstalledCLIs = func() []string {
	out := []string{}
	for _, c := range skills.Detect("") {
		if c.Installed {
			out = append(out, c.Name)
		}
	}
	return out
}

// IsValid corre las validaciones que pide H6 antes de "guardar pipeline"
// en S3. Errores en castellano para que el usuario los lea inline en S3.
//
// Se cubren:
//   - name vacio
//   - sin steps
//   - duplicates de step name (paranoia: buildStep ya lo previene en S2,
//     pero un draft cargado desde disco puede llegar con el invariante roto).
//   - previous_output en step 0 (no hay step previo).
//   - validator con CLI no instalado (mismo motivo: en S2 se valida al
//     guardar, pero un draft viejo puede tener un CLI desinstalado a posteriori).
//
// Devuelve nil cuando todo pasa. Cuando falla, devuelve un error con
// mensaje multi-linea: cada linea empieza con "- " para que el caller la
// pueda partir y mostrar como lista (S3 hace exactamente eso).
func IsValid(p Pipeline) error {
	var errs []string

	if p.Name == "" {
		errs = append(errs, "el nombre del pipeline no puede estar vacio")
	}
	if len(p.Steps) == 0 {
		errs = append(errs, "el pipeline necesita al menos un step")
	}

	seen := map[string]int{}
	for i, s := range p.Steps {
		if s.Name == "" {
			errs = append(errs, fmt.Sprintf("step %d: nombre vacio", i+1))
			continue
		}
		if first, ok := seen[s.Name]; ok {
			errs = append(errs, fmt.Sprintf("step %d (%q) duplica el nombre del step %d", i+1, s.Name, first+1))
		} else {
			seen[s.Name] = i
		}
	}

	for i, s := range p.Steps {
		if i == 0 && s.Input == InputPreviousOutput {
			errs = append(errs, fmt.Sprintf("step %d: input=previous_output no se puede usar en el primer step", i+1))
		}
	}

	installed := map[string]bool{}
	for _, name := range detectInstalledCLIs() {
		installed[name] = true
	}
	for i, s := range p.Steps {
		if s.CLI != "" && !installed[s.CLI] {
			errs = append(errs, fmt.Sprintf("step %d: %s no esta instalado — corre `che doctor` o cambia el CLI", i+1, s.CLI))
		}
		if s.Validator != nil && s.Validator.CLI != "" && !installed[s.Validator.CLI] {
			errs = append(errs, fmt.Sprintf("step %d: validator %s no esta instalado — corre `che doctor` o cambia el CLI", i+1, s.Validator.CLI))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return validationError(errs)
}

// validationError es un error con varios mensajes; el caller lo expande a
// una lista al renderizar. Implementa error con la concatenacion canonica
// "- a\n- b" para que `err.Error()` siga siendo util fuera de S3.
type validationError []string

func (v validationError) Error() string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += "\n"
		}
		out += "- " + s
	}
	return out
}

// Lines expone los mensajes individuales para renderizado. Si el error no
// es de tipo validationError devuelve un slice de un solo elemento con el
// texto completo — asi el caller no necesita type-switch.
func validationLines(err error) []string {
	if err == nil {
		return nil
	}
	if v, ok := err.(validationError); ok {
		return []string(v)
	}
	return []string{err.Error()}
}
