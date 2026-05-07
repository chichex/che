package wizard

import (
	"strings"
	"unicode/utf8"
)

// textInput es un input de texto minimal: buffer de runas + cursor +
// flag multiline. Sin scroll, sin paste, sin undo — alcanza para los
// dos campos de S1. La forma "pro" (con bubbles/textinput) puede llegar
// despues sin tocar callers.
type textInput struct {
	runes     []rune
	cursor    int
	multiline bool
	// placeholder se renderiza cuando el buffer esta vacio y el input
	// tiene foco; sin foco se respeta tambien para hint visual.
	placeholder string
}

func newSingleLine(placeholder string) textInput {
	return textInput{placeholder: placeholder}
}

func newMultiLine(placeholder string) textInput {
	return textInput{multiline: true, placeholder: placeholder}
}

// Value devuelve el texto actual.
func (t textInput) Value() string {
	return string(t.runes)
}

// SetValue reemplaza el contenido y mueve el cursor al final.
func (t *textInput) SetValue(s string) {
	t.runes = []rune(s)
	t.cursor = len(t.runes)
}

// HandleKey aplica una tecla bubbletea al input. Devuelve true si el input
// consumio la tecla (caller no debe seguir interpretando).
func (t *textInput) HandleKey(key string, runesIn []rune) bool {
	switch key {
	case "left":
		if t.cursor > 0 {
			t.cursor--
		}
		return true
	case "right":
		if t.cursor < len(t.runes) {
			t.cursor++
		}
		return true
	case "home", "ctrl+a":
		t.cursor = 0
		return true
	case "end", "ctrl+e":
		t.cursor = len(t.runes)
		return true
	case "backspace", "ctrl+h":
		if t.cursor > 0 {
			t.runes = append(t.runes[:t.cursor-1], t.runes[t.cursor:]...)
			t.cursor--
		}
		return true
	case "delete":
		if t.cursor < len(t.runes) {
			t.runes = append(t.runes[:t.cursor], t.runes[t.cursor+1:]...)
		}
		return true
	case "enter":
		// enter no inserta newline en ningun caso — siempre avanza foco
		// (lo maneja el wizard). Para newline literal en multiline,
		// shift+enter / alt+enter / ctrl+j.
		return false
	case "shift+enter", "alt+enter", "ctrl+j":
		if t.multiline {
			t.insertRune('\n')
			return true
		}
		return false
	case "tab", "shift+tab", "esc", "ctrl+c", "ctrl+s", "ctrl+n", "up", "down":
		// teclas reservadas al wizard (foco, transiciones, cancel).
		return false
	}

	// Tecla "normal": insertamos las runas sin modificadores. tea pasa
	// runesIn = nil para teclas no-character.
	if len(runesIn) == 0 {
		return false
	}
	for _, r := range runesIn {
		if !t.multiline && (r == '\n' || r == '\r') {
			continue
		}
		// filtrar control chars (except newline en multiline ya manejado)
		if r < 0x20 && r != '\n' {
			continue
		}
		t.insertRune(r)
	}
	return true
}

func (t *textInput) insertRune(r rune) {
	t.runes = append(t.runes, 0)
	copy(t.runes[t.cursor+1:], t.runes[t.cursor:])
	t.runes[t.cursor] = r
	t.cursor++
}

// view renderiza el input con cursor visible si focused. No estiliza —
// quien llama envuelve con lipgloss para borde / colores.
func (t textInput) view(focused bool) string {
	if len(t.runes) == 0 {
		if t.placeholder != "" {
			if focused {
				return cursorBlock + placeholderText(t.placeholder)
			}
			return placeholderText(t.placeholder)
		}
		if focused {
			return cursorBlock
		}
		return " "
	}

	if !focused {
		return string(t.runes)
	}

	var b strings.Builder
	if t.cursor >= len(t.runes) {
		b.WriteString(string(t.runes))
		b.WriteString(cursorBlock)
		return b.String()
	}
	b.WriteString(string(t.runes[:t.cursor]))
	b.WriteString(cursorBlock)
	// resto despues del cursor
	rest := string(t.runes[t.cursor:])
	// si la primera rune del resto fuera el cursor, no hay choque porque
	// cursorBlock es un caracter dedicado. utf8 sigue valido.
	_ = utf8.RuneCountInString(rest)
	b.WriteString(rest)
	return b.String()
}
