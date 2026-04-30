package pipeline

// Default devuelve el pipeline built-in que reproduce el flujo histórico de
// che (idea → explore → validate → execute → validate → close) en formato
// declarativo. Es el fallback cuando el repo no tiene `.che/pipelines/` ni
// `pipelines.config.json`: el comportamiento por defecto sigue siendo el
// que los usuarios ya conocen — la feature de pipelines es opt-in.
//
// Mantenerlo en memoria (no como archivo embebido) permite que el binario
// no necesite cargar IO en arranque y que el comando `che pipeline new`
// pueda materializar este pipeline a disco como template inicial.
//
// El JSON canónico de este pipeline está duplicado en el golden de tests
// (`testdata/default.json`). Si cambia esta función, regenerar el golden;
// si cambia el golden, regenerar esta función. El test verifica drift
// bidireccional.
//
// Notas sobre los agentes referenciados:
//   - `claude-opus` es uno de los 3 built-in (PRD §2.b), siempre disponible.
//   - `plan-reviewer-strict` / `plan-reviewer-pragmatic` /
//     `code-reviewer-strict` / `code-reviewer-security` son agentes
//     CUSTOM esperados — el usuario los define en `.claude/agents/`. Si
//     no existen, validate del pipeline reportará el dangling ref. El
//     built-in `Default()` los referencia porque es el shape canónico
//     mostrado en el PRD §4.b; un usuario sin esos agentes puede clonar
//     el default y reemplazarlos via `che pipeline clone default mine
//     --replace plan-reviewer-strict=claude-opus`.
//
// El loop validate ↔ explore (o validate_pr ↔ execute) NO se declara en el
// JSON: vive en el system prompt de los agentes `*-reviewer-*` vía markers
// `[goto: <step>]`. Por eso este pipeline luce lineal en el JSON pero
// reproduce iteración real cuando los agentes están bien configurados.
func Default() Pipeline {
	return Pipeline{
		Version: CurrentVersion,
		Steps: []Step{
			{
				Name:   "idea",
				Agents: []string{"claude-opus"},
			},
			{
				Name:   "explore",
				Agents: []string{"claude-opus"},
			},
			{
				Name: "validate_issue",
				Agents: []string{
					"plan-reviewer-strict",
					"plan-reviewer-pragmatic",
					"claude-opus",
				},
			},
			{
				Name:   "execute",
				Agents: []string{"claude-opus"},
			},
			{
				Name: "validate_pr",
				Agents: []string{
					"code-reviewer-strict",
					"code-reviewer-security",
					"claude-opus",
				},
			},
			{
				Name:   "close",
				Agents: []string{"claude-opus"},
			},
		},
	}
}
