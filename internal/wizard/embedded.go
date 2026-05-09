package wizard

import (
	_ "embed"
	"fmt"
)

// builtinCheFunnelYAML es el pipeline default `che-funnel` shippeado con el
// binario. Replica el embudo idea -> plan -> ejecucion -> close del che
// clasico (v0.0.82) usando solo el modelo nuevo (cli + kind + content +
// validator). Toda la state machine de GitHub la dirigen los prompts via
// tool use de gh/git.
//
//go:embed embedded/che-funnel.yaml
var builtinCheFunnelYAML []byte

// BuiltinPipeline es un pipeline shippeado con el binario. Aparece siempre
// en "My pipelines" con chip [default]. Source guarda el YAML serializado
// para soportar copy-on-edit (escribir el archivo a ~/.che/pipelines/<slug>
// .yaml cuando el usuario quiere personalizarlo). Pipeline ya viene parseado
// para evitar re-parsear en cada keystroke del lister.
type BuiltinPipeline struct {
	Slug     string
	Source   []byte
	Pipeline Pipeline
}

// Builtins devuelve los pipelines embedded. Si alguno falla al parsear es
// bug del binario (no del usuario), por eso devolvemos error en vez de
// skipear: el lister va a surfacearlo como "default invalido — bug del
// binario, reportar".
func Builtins() ([]BuiltinPipeline, error) {
	raw := []struct {
		slug string
		data []byte
	}{
		{"che-funnel", builtinCheFunnelYAML},
	}
	out := make([]BuiltinPipeline, 0, len(raw))
	for _, r := range raw {
		p, err := Unmarshal(r.data)
		if err != nil {
			return nil, fmt.Errorf("wizard: builtin %s: %w", r.slug, err)
		}
		out = append(out, BuiltinPipeline{
			Slug:     r.slug,
			Source:   r.data,
			Pipeline: p,
		})
	}
	return out, nil
}
