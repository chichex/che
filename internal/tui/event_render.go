package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/chichex/che/internal/output"
)

// renderEventLine convierte un Event del logger a una linea estilizada
// para el runLog de la TUI.
//
// Reusa la paleta de styles.go (colorSuccess, colorError, colorAccent,
// colorMuted, colorPrimary) para quedar visualmente consistente con el
// resto de la TUI. El simbolo y los fields se alinean igual que el CLI.
func renderEventLine(ev output.Event) string {
	sym := symbolForLevel(ev.Level)
	symStyle := levelStyle(ev.Level)
	body := symStyle.Render(sym) + " " + symStyle.Render(ev.Message)

	if suffix := renderEventFields(ev.Fields); suffix != "" {
		body += " " + suffix
	}
	return body
}

func symbolForLevel(lv output.Level) string {
	switch lv {
	case output.LevelInfo:
		return "▸"
	case output.LevelStep:
		return "·"
	case output.LevelSuccess:
		return "✓"
	case output.LevelWarn:
		return "⚠"
	case output.LevelError:
		return "✗"
	}
	return "▸"
}

func levelStyle(lv output.Level) lipgloss.Style {
	switch lv {
	case output.LevelInfo:
		return lipgloss.NewStyle().Foreground(colorText)
	case output.LevelStep:
		return logLineStyle
	case output.LevelSuccess:
		return successStyle
	case output.LevelWarn:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C"))
	case output.LevelError:
		return errorStyle
	}
	return logLineStyle
}

// renderPayloadLine estiliza las lineas de stdout "payload" de los flows
// (Done., Executed URL, PR: URL, etc.) para que destaquen frente al runLog
// del agente — que va en logLineStyle muted. Sin esto, el resultado final
// queda indistinguible del ruido de pasos.
//
// Lipgloss anida ANSI: aunque el caller envuelva con logLineStyle.Render,
// los escapes internos de successStyle/payloadPrimary sobreviven.
func renderPayloadLine(line string) string {
	trim := strings.TrimSpace(line)

	// Prefijos de "lo logré": verbo en pretérito + Done.
	successPrefixes := []string{"Done", "Executed ", "Explored ", "Created ", "Creating ", "Iterated ", "Closed ", "Cerrado"}
	for _, p := range successPrefixes {
		if strings.HasPrefix(trim, p) {
			return lipgloss.NewStyle().Foreground(colorSuccess).Bold(true).Render(line)
		}
	}

	// Links accionables: label en muted, URL en cian bold.
	linkPrefixes := []string{"PR: ", "Comment: "}
	for _, p := range linkPrefixes {
		if strings.HasPrefix(trim, p) {
			label := trim[:len(p)]
			rest := trim[len(p):]
			mutedLabel := lipgloss.NewStyle().Foreground(colorMuted).Render(label)
			url := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true).Underline(true).Render(rest)
			return mutedLabel + url
		}
	}

	// Resto (p.ej. "Nuevos commits: 3", reporte de validate): off-white
	// para que se lea bien sin competir con los items destacados.
	return lipgloss.NewStyle().Foreground(colorText).Render(line)
}

func renderEventFields(f output.F) string {
	var parts []string
	num := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	muted := lipgloss.NewStyle().Foreground(colorMuted)
	labels := lipgloss.NewStyle().Foreground(colorAccent)
	agent := lipgloss.NewStyle().Foreground(colorPrimary).Bold(true)
	verdictOK := lipgloss.NewStyle().Foreground(colorSuccess).Bold(true)
	verdictKO := lipgloss.NewStyle().Foreground(colorError).Bold(true)

	if f.Issue > 0 {
		parts = append(parts, num.Render(fmt.Sprintf("#%d", f.Issue)))
	}
	if f.PR > 0 {
		parts = append(parts, muted.Render("PR")+" "+num.Render(fmt.Sprintf("#%d", f.PR)))
	}
	if len(f.Labels) > 0 {
		colored := make([]string, len(f.Labels))
		for i, l := range f.Labels {
			colored[i] = labels.Render(l)
		}
		parts = append(parts, muted.Render("[")+strings.Join(colored, muted.Render(", "))+muted.Render("]"))
	}
	if f.Iter > 0 {
		parts = append(parts, muted.Render(fmt.Sprintf("iter=%d", f.Iter)))
	}
	if f.Agent != "" {
		parts = append(parts, agent.Render("{"+f.Agent+"}"))
	}
	if f.Validator != "" {
		parts = append(parts, agent.Render("{"+f.Validator+"}"))
	}
	if f.Attempt > 0 && f.Total > 0 {
		parts = append(parts, muted.Render(fmt.Sprintf("(intento %d/%d)", f.Attempt, f.Total)))
	}
	if f.Verdict != "" {
		verdictStyle := muted
		switch strings.ToLower(strings.TrimSpace(f.Verdict)) {
		case "approve":
			verdictStyle = verdictOK
		case "changes_requested", "changes-requested", "needs_human", "needs-human":
			verdictStyle = verdictKO
		}
		parts = append(parts, muted.Render("verdict:")+" "+verdictStyle.Render(f.Verdict))
	}
	if f.Cause != nil {
		parts = append(parts, muted.Render("—")+" "+errorStyle.Render("error: "+f.Cause.Error()))
	}
	if f.Detail != "" {
		parts = append(parts, muted.Render("("+f.Detail+")"))
	}
	if f.URL != "" {
		parts = append(parts, muted.Render("·")+" "+muted.Render(f.URL))
	}
	return strings.Join(parts, " ")
}
