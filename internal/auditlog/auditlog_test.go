package auditlog

import (
	"strings"
	"testing"
	"time"
)

// TestAppend_CreatesNewComment: el issue no tiene un comment con el marker
// → Append llama a CreateComment con el body completo (marker + título +
// primera entrada). EditComment no se invoca.
func TestAppend_CreatesNewComment(t *testing.T) {
	var createdBody string
	editCalls := 0

	id, err := Append(42, Entry{
		At:   time.Date(2024, 12, 1, 10, 23, 45, 0, time.UTC),
		Flow: "explore",
		From: "che:state:idea",
		To:   "che:state:applying:explore",
	}, Options{
		ListComments: func(int) ([]Comment, error) { return nil, nil },
		CreateComment: func(_ int, body string) (int64, error) {
			createdBody = body
			return 999, nil
		},
		EditComment: func(int64, string) error {
			editCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id != 999 {
		t.Errorf("id = %d, want 999", id)
	}
	if editCalls != 0 {
		t.Errorf("editCalls = %d, want 0 (no había comment previo)", editCalls)
	}
	if !strings.Contains(createdBody, Marker) {
		t.Errorf("body sin marker:\n%s", createdBody)
	}
	if !strings.Contains(createdBody, Title) {
		t.Errorf("body sin título:\n%s", createdBody)
	}
	if !strings.Contains(createdBody, "explore") || !strings.Contains(createdBody, "che:state:idea") {
		t.Errorf("body sin línea de evento:\n%s", createdBody)
	}
}

// TestAppend_EditsExistingComment: ya existe un comment con el marker →
// Append edita el body, no crea un comment nuevo. El body resultante
// preserva el contenido viejo + appendea la nueva línea.
func TestAppend_EditsExistingComment(t *testing.T) {
	existingBody := Marker + "\n" + Title + "\n\n- 2024-12-01T10:00:00Z · idea · - → che:state:idea"
	createCalls := 0
	var editID int64
	var editBody string

	id, err := Append(42, Entry{
		At:   time.Date(2024, 12, 1, 10, 23, 45, 0, time.UTC),
		Flow: "explore",
		From: "che:state:idea",
		To:   "che:state:applying:explore",
	}, Options{
		ListComments: func(int) ([]Comment, error) {
			return []Comment{
				{ID: 11, Body: "comment de un humano sin marker"},
				{ID: 22, Body: existingBody},
				{ID: 33, Body: "otro comment sin marker"},
			}, nil
		},
		CreateComment: func(int, string) (int64, error) {
			createCalls++
			return 0, nil
		},
		EditComment: func(cid int64, body string) error {
			editID = cid
			editBody = body
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if id != 22 {
		t.Errorf("id devuelto = %d, want 22 (comment con marker)", id)
	}
	if createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 (debe editar, no crear)", createCalls)
	}
	if editID != 22 {
		t.Errorf("editID = %d, want 22", editID)
	}
	if !strings.Contains(editBody, "che:state:idea → che:state:applying:explore") {
		t.Errorf("editBody no contiene la nueva entrada:\n%s", editBody)
	}
	// Preserva la entrada vieja.
	if !strings.Contains(editBody, "10:00:00Z") {
		t.Errorf("editBody no preserva la entrada vieja:\n%s", editBody)
	}
}

// TestRenderEntry_FormatVariations cubre los tres shapes principales:
// transición completa, sin To, sin From ni To pero con flow + note.
func TestRenderEntry_FormatVariations(t *testing.T) {
	at := time.Date(2024, 12, 1, 10, 23, 45, 0, time.UTC)
	cases := []struct {
		name string
		in   Entry
		want string
	}{
		{
			name: "transition",
			in:   Entry{At: at, Flow: "explore", From: "che:state:idea", To: "che:state:applying:explore"},
			want: "- 2024-12-01T10:23:45Z · explore · che:state:idea → che:state:applying:explore",
		},
		{
			name: "rollback note",
			in:   Entry{At: at, Flow: "explore", From: "che:state:applying:explore", To: "che:state:idea", Note: "rollback"},
			want: "- 2024-12-01T10:23:45Z · explore · che:state:applying:explore → che:state:idea (rollback)",
		},
		{
			name: "no destination",
			in:   Entry{At: at, Flow: "lock", From: "acquired", Note: "pid=12345"},
			want: "- 2024-12-01T10:23:45Z · lock · acquired (pid=12345)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderEntry(c.in)
			if got != c.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, c.want)
			}
		})
	}
}

// TestAppend_AtZeroFillsNow: si entry.At es zero, Append usa Now(). Tests
// inyectan un Now fijo para verificar que la línea generada lleva ese
// timestamp.
func TestAppend_AtZeroFillsNow(t *testing.T) {
	fixedNow := time.Date(2025, 5, 3, 12, 0, 0, 0, time.UTC)
	var captured string
	_, err := Append(42, Entry{
		Flow: "execute",
		From: "che:state:explore",
		To:   "che:state:applying:execute",
	}, Options{
		Now:           func() time.Time { return fixedNow },
		ListComments:  func(int) ([]Comment, error) { return nil, nil },
		CreateComment: func(_ int, body string) (int64, error) { captured = body; return 1, nil },
		EditComment:   func(int64, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !strings.Contains(captured, "2025-05-03T12:00:00Z") {
		t.Errorf("body sin timestamp inyectado:\n%s", captured)
	}
}

// TestAppend_PreservesTrailingNewlinesGracefully: si el body existente
// tiene trailing newlines, Append no acumula líneas vacías al editar.
func TestAppend_PreservesTrailingNewlinesGracefully(t *testing.T) {
	existing := Marker + "\n" + Title + "\n\n- 2024-12-01T10:00:00Z · idea\n\n\n"
	var got string
	_, err := Append(42, Entry{
		At: time.Date(2024, 12, 1, 10, 23, 45, 0, time.UTC), Flow: "x", From: "a", To: "b",
	}, Options{
		ListComments: func(int) ([]Comment, error) {
			return []Comment{{ID: 22, Body: existing}}, nil
		},
		CreateComment: func(int, string) (int64, error) { return 0, nil },
		EditComment:   func(_ int64, b string) error { got = b; return nil },
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	// No debe haber 3+ newlines consecutivas justo antes de la nueva entrada.
	if strings.Contains(got, "\n\n\n-") {
		t.Errorf("trailing newlines acumuladas:\n%q", got)
	}
}
