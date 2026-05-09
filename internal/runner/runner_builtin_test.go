package runner

import (
	"testing"
)

// TestLoadPipelineForRun_BuiltinSentinel verifica que loadPipelineForRun
// resuelve el sentinel "builtin:<slug>" via wizard.BuiltinBySlug en vez
// de tocar el FS. Cubre el caso por el que el lister devuelve targets
// builtin para enter sobre rows embedded.
func TestLoadPipelineForRun_BuiltinSentinel(t *testing.T) {
	p, err := loadPipelineForRun("builtin:che-funnel")
	if err != nil {
		t.Fatalf("loadPipelineForRun(builtin:che-funnel): %v", err)
	}
	if p.Name != "che-funnel" {
		t.Errorf("Name: got %q want che-funnel", p.Name)
	}
	if len(p.Steps) == 0 {
		t.Fatal("Steps vacio")
	}
	// Sanity: el primer step del builtin es 'idea'.
	if p.Steps[0].Name != "idea" {
		t.Errorf("Steps[0].Name: got %q want idea", p.Steps[0].Name)
	}
}

func TestLoadPipelineForRun_UnknownBuiltinError(t *testing.T) {
	_, err := loadPipelineForRun("builtin:no-existe")
	if err == nil {
		t.Fatal("esperaba error para builtin desconocido, got nil")
	}
}

func TestLoadPipelineForRun_FilesystemPathDelegatesToWizardLoad(t *testing.T) {
	// Caso negativo: path inexistente del FS debe devolver el error de
	// wizard.Load envuelto con prefijo "runner: load". Confirma que el
	// branch del FS sigue activo.
	_, err := loadPipelineForRun("/no/existe.yaml")
	if err == nil {
		t.Fatal("esperaba error para path inexistente, got nil")
	}
}

// IsValid del builtin se cubre en internal/wizard/embedded_test.go
// (TestBuiltins_IsValidWithMockedCLIs). Aca solo testeamos el branching
// del runner.
