package runner

import (
	"strings"
	"testing"
)

// TestDefaultModel cubre los 4 CLIs primer-class + uno desconocido.
func TestDefaultModel(t *testing.T) {
	cases := []struct {
		cli  string
		want string
	}{
		{"claude", "opus"},
		{"codex", "gpt-5.5"},
		{"gemini", "gemini-2.5-pro"},
		{"opencode", ""},
		{"future-cli", ""},
	}
	for _, tc := range cases {
		t.Run(tc.cli, func(t *testing.T) {
			if got := DefaultModel(tc.cli); got != tc.want {
				t.Errorf("DefaultModel(%q) = %q, want %q", tc.cli, got, tc.want)
			}
		})
	}
}

// TestValidateModel_EmptyAlwaysPasses garantiza que model=="" pasa para
// cualquier CLI (rama "sin override" → cae al default).
func TestValidateModel_EmptyAlwaysPasses(t *testing.T) {
	for _, cli := range []string{"claude", "codex", "gemini", "opencode", "future-cli"} {
		t.Run(cli, func(t *testing.T) {
			if err := ValidateModel(cli, ""); err != nil {
				t.Errorf("ValidateModel(%q, \"\") = %v, want nil", cli, err)
			}
		})
	}
}

// TestValidateModel_OpencodeRejectsAnyModel garantiza que opencode rechaza
// cualquier modelo declarado en YAML (regla del body de #142).
func TestValidateModel_OpencodeRejectsAnyModel(t *testing.T) {
	err := ValidateModel("opencode", "claude-3-7-sonnet")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "opencode no soporta override") {
		t.Errorf("error message debe explicar la regla; got: %v", err)
	}
}

// TestValidateModel_WhitelistedClaude cubre los aliases + nombres completos
// declarados en modelsByCLI.
func TestValidateModel_WhitelistedClaude(t *testing.T) {
	valid := []string{"opus", "sonnet", "haiku", "opusplan", "claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5"}
	for _, m := range valid {
		t.Run(m, func(t *testing.T) {
			if err := ValidateModel("claude", m); err != nil {
				t.Errorf("ValidateModel(claude, %q) = %v, want nil", m, err)
			}
		})
	}
}

// TestValidateModel_WhitelistedCodex cubre los modelos codex declarados.
func TestValidateModel_WhitelistedCodex(t *testing.T) {
	valid := []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-5.2-codex", "gpt-5.1-codex-max", "gpt-5.1-codex-mini"}
	for _, m := range valid {
		t.Run(m, func(t *testing.T) {
			if err := ValidateModel("codex", m); err != nil {
				t.Errorf("ValidateModel(codex, %q) = %v, want nil", m, err)
			}
		})
	}
}

// TestValidateModel_WhitelistedGemini cubre los modelos gemini declarados.
func TestValidateModel_WhitelistedGemini(t *testing.T) {
	valid := []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite"}
	for _, m := range valid {
		t.Run(m, func(t *testing.T) {
			if err := ValidateModel("gemini", m); err != nil {
				t.Errorf("ValidateModel(gemini, %q) = %v, want nil", m, err)
			}
		})
	}
}

// TestValidateModel_InvalidPerCLI cubre rechazo de modelo no listado para
// claude/codex/gemini. El error debe mencionar el CLI + el modelo + la
// lista de aceptados para que el remedy del preflight sea accionable.
func TestValidateModel_InvalidPerCLI(t *testing.T) {
	cases := []struct {
		cli   string
		model string
	}{
		{"claude", "gpt-4o"},     // modelo de otro CLI
		{"codex", "opus"},        // alias de claude
		{"gemini", "gpt-5.5"},    // modelo de codex
		{"claude", "OPUS"},       // case-sensitive — debe rechazar
		{"gemini", "gemini-pro"}, // nombre viejo no listado
	}
	for _, tc := range cases {
		t.Run(tc.cli+"/"+tc.model, func(t *testing.T) {
			err := ValidateModel(tc.cli, tc.model)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.cli) {
				t.Errorf("error debe mencionar cli %q; got: %v", tc.cli, err)
			}
			if !strings.Contains(msg, tc.model) {
				t.Errorf("error debe mencionar modelo %q; got: %v", tc.model, err)
			}
			if !strings.Contains(msg, "aceptados") {
				t.Errorf("error debe listar modelos aceptados; got: %v", err)
			}
		})
	}
}

// TestValidateModel_UnknownCLIPasses garantiza que CLIs no listados en
// modelsByCLI (y distintos de opencode) pasan sin chequeo — abre la puerta
// a CLIs custom sin tocar la tabla.
func TestValidateModel_UnknownCLIPasses(t *testing.T) {
	if err := ValidateModel("future-cli", "any-model"); err != nil {
		t.Errorf("ValidateModel(future-cli, any-model) = %v, want nil", err)
	}
}
