package wizard

// pipelinesEquivalentContent compara dos pipelines ignorando el bloque
// status (stage / step_idx / step_mode / last_saved_at). Sirve para
// detectar "no hubo cambios reales en el contenido del pipeline" antes de
// un Save redundante: el bloque status describe donde quedo el wizard, no
// que tiene el pipeline, asi que diferencias solo en status no justifican
// reescribir el archivo.
//
// Caso motivador: edit-ready siembra status.stage=summary en RAM para que
// el wizard trate el archivo como draft, pero el archivo en disco sigue
// ready (sin status). Si el usuario sale con SC keep sin haber tocado
// nada, este chequeo hace que el Save se skipee y el archivo siga ready.
func pipelinesEquivalentContent(a, b Pipeline) bool {
	if a.Name != b.Name {
		return false
	}
	if a.Description != b.Description {
		return false
	}
	return stepsEqualAll(a.Steps, b.Steps)
}

func stepsEqualAll(a, b []Step) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !stepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func stepEqual(a, b Step) bool {
	if a.Name != b.Name {
		return false
	}
	if a.CLI != b.CLI {
		return false
	}
	if a.Model != b.Model {
		return false
	}
	if a.Kind != b.Kind {
		return false
	}
	if a.Content != b.Content {
		return false
	}
	if a.Input != b.Input {
		return false
	}
	if a.MaxLoops != b.MaxLoops {
		return false
	}
	if a.OnMaxLoops != b.OnMaxLoops {
		return false
	}
	return validatorEqual(a.Validator, b.Validator)
}

func validatorEqual(a, b *Validator) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.CLI == b.CLI && a.Kind == b.Kind && a.Content == b.Content && a.Model == b.Model
}
