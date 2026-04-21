// Package comments centraliza el formato de los headers estructurados que che
// embebe al inicio de cada comment (issue o PR) como HTML comment
// `<!-- claude-cli: k=v ... -->`. El header permite al flow parsear comments
// históricos y saber qué iteración / agente / rol los produjo — necesario, por
// ejemplo, para el ciclo iter de execute con scope-lock (design.html L1257+).
package comments

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Header es la metadata parseada del HTML comment de che al inicio del body.
// Si Role es "" el body no tiene header (es del humano o de otra herramienta).
type Header struct {
	Flow     string
	Iter     int
	Agent    string
	Instance int
	Role     string
}

var headerRe = regexp.MustCompile(`^<!--\s*claude-cli:\s*(.+?)\s*-->`)
var kvRe = regexp.MustCompile(`(\w+)=(\S+)`)

// Parse lee la primera línea del body y, si es un HTML comment de che,
// devuelve la metadata. Si no lo es, devuelve un Header vacío.
func Parse(body string) Header {
	m := headerRe.FindStringSubmatch(strings.TrimSpace(body))
	if m == nil {
		return Header{}
	}
	h := Header{}
	for _, kv := range kvRe.FindAllStringSubmatch(m[1], -1) {
		switch kv[1] {
		case "flow":
			h.Flow = kv[2]
		case "iter":
			if n, err := strconv.Atoi(kv[2]); err == nil {
				h.Iter = n
			}
		case "agent":
			h.Agent = kv[2]
		case "instance":
			if n, err := strconv.Atoi(kv[2]); err == nil {
				h.Instance = n
			}
		case "role":
			h.Role = kv[2]
		}
	}
	return h
}

// Format devuelve el HTML comment serializado listo para prependerse al body
// (sin newline final). Campos zero se omiten así el resultado no mete
// `iter=0` ni `instance=0` cuando no aplican.
func (h Header) Format() string {
	var parts []string
	if h.Flow != "" {
		parts = append(parts, "flow="+h.Flow)
	}
	if h.Iter > 0 {
		parts = append(parts, fmt.Sprintf("iter=%d", h.Iter))
	}
	if h.Agent != "" {
		parts = append(parts, "agent="+h.Agent)
	}
	if h.Instance > 0 {
		parts = append(parts, fmt.Sprintf("instance=%d", h.Instance))
	}
	if h.Role != "" {
		parts = append(parts, "role="+h.Role)
	}
	return "<!-- claude-cli: " + strings.Join(parts, " ") + " -->"
}
