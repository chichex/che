package tui

import (
	"strings"
	"testing"
)

// TestViewIncludesVersion verifica que el menu principal renderiza la
// version recibida por parametro como "v<version>" — asi el usuario sabe
// que binario esta corriendo sin salir del TUI. El render usa dimStyle, que
// emite secuencias ANSI; el sub-string "v1.2.3" igual aparece literal en el
// output porque dimStyle no transforma el contenido.
func TestViewIncludesVersion(t *testing.T) {
	m := model{version: "1.2.3"}
	out := m.View()
	if !strings.Contains(out, "v1.2.3") {
		t.Fatalf("View() output no contiene la version esperada %q; got:\n%s", "v1.2.3", out)
	}
}
