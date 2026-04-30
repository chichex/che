package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadError describe un fallo de parse o validación de un pipeline.
//
// Lleva Path + Line + Column + Field cuando el origen del error los
// permite computar (typically: errores de JSON syntax via
// `json.SyntaxError.Offset`, errores de tipo via
// `json.UnmarshalTypeError`, validación semántica con la ruta JSON del
// campo). Los campos que no se pudieron determinar quedan en cero.
//
// Mantener un tipo único de error simplifica el call site: los flows
// que muestran feedback al humano hacen `errors.As(err, &le)` y formatean
// según los campos presentes — no necesitan distinguir entre "JSON
// inválido" y "agente desconocido", el mensaje compuesto ya lo dice.
type LoadError struct {
	// Path es el archivo donde ocurrió el error. Vacío cuando el caller
	// llamó LoadBytes (no hay path conocido).
	Path string

	// Line / Column son 1-based. 0 = desconocido (validación semántica
	// post-decode no tiene offset).
	Line   int
	Column int

	// Field es la ruta JSON del campo culpable (ej. "steps[2].agents[1]").
	// Vacío si el error no se asocia a un campo puntual.
	Field string

	// Reason es la descripción humana sin metadatos.
	Reason string
}

func (e *LoadError) Error() string {
	var sb strings.Builder
	if e.Path != "" {
		sb.WriteString(e.Path)
		if e.Line > 0 {
			fmt.Fprintf(&sb, ":%d", e.Line)
			if e.Column > 0 {
				fmt.Fprintf(&sb, ":%d", e.Column)
			}
		}
		sb.WriteString(": ")
	}
	if e.Field != "" {
		fmt.Fprintf(&sb, "field %q: ", e.Field)
	}
	sb.WriteString(e.Reason)
	return sb.String()
}

// Load lee y valida un archivo de pipeline JSON. Devuelve un *LoadError
// con Path + Line/Column/Field rellenos cuando aplica.
func Load(path string) (Pipeline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Pipeline{}, &LoadError{Path: path, Reason: err.Error()}
	}
	p, err := LoadBytes(data)
	if err != nil {
		var le *LoadError
		if errors.As(err, &le) {
			le.Path = path
			return Pipeline{}, le
		}
		return Pipeline{}, &LoadError{Path: path, Reason: err.Error()}
	}
	return p, nil
}

// LoadBytes parsea + valida un pipeline desde un slice de bytes (sin
// path asociado). Útil para tests, para `che pipeline simulate` con
// stdin, o para previews del wizard que aún no escribió a disco.
func LoadBytes(data []byte) (Pipeline, error) {
	p, err := decodePipeline(data)
	if err != nil {
		return Pipeline{}, err
	}
	if err := Validate(p); err != nil {
		return Pipeline{}, err
	}
	return p, nil
}

// decodePipeline corre json.Decoder en strict mode (DisallowUnknownFields)
// y traduce los errores nativos a *LoadError con Line/Column/Field.
//
// Strict mode espeja el `additionalProperties: false` del schema
// (`schemas/pipeline.json`): si el archivo declara campos desconocidos
// es un typo o un cambio de versión que el loader no conoce, no algo
// que valga la pena tolerar silenciosamente.
func decodePipeline(data []byte) (Pipeline, error) {
	var p Pipeline
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Pipeline{}, jsonDecodeError(err, data)
	}
	// Detectar tokens extra después del primer valor — un .json válido
	// es un único objeto, basura adicional indica concatenación
	// accidental o un export erróneo.
	if dec.More() {
		return Pipeline{}, &LoadError{Reason: "trailing data after pipeline JSON object"}
	}
	return p, nil
}

// jsonDecodeError convierte errores de encoding/json a *LoadError con
// Line/Column derivados del Offset y Field cuando lo provee
// json.UnmarshalTypeError.
//
// Para "unknown field" no hay un offset accesible (encoding/json no
// expone la posición), así que quedan sin Line/Column pero con Field
// extraído del mensaje.
func jsonDecodeError(err error, data []byte) *LoadError {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		line, col := offsetToLineCol(data, int(syntaxErr.Offset))
		return &LoadError{
			Line:   line,
			Column: col,
			Reason: fmt.Sprintf("invalid JSON: %s", syntaxErr.Error()),
		}
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		line, col := offsetToLineCol(data, int(typeErr.Offset))
		field := typeErr.Field
		if field == "" {
			field = typeErr.Type.String()
		}
		return &LoadError{
			Line:   line,
			Column: col,
			Field:  field,
			Reason: fmt.Sprintf("expected %s, got %s", typeErr.Type, typeErr.Value),
		}
	}
	if field, ok := unknownFieldName(err); ok {
		return &LoadError{
			Field:  field,
			Reason: fmt.Sprintf("unknown field %q (loader rejects unknown fields; check spelling or upgrade che)", field),
		}
	}
	return &LoadError{Reason: err.Error()}
}

// unknownFieldName extrae el nombre del campo del mensaje
// "json: unknown field \"X\"". encoding/json no expone un tipo
// dedicado para este error, así que parseamos el string. Frágil pero
// estable: el mensaje no cambió en años. Si cambia en una versión
// futura, el LoadError fallback con Reason completo sigue siendo útil.
func unknownFieldName(err error) (string, bool) {
	const prefix = "json: unknown field "
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(msg, prefix)
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
		return rest[1 : len(rest)-1], true
	}
	return rest, true
}

// offsetToLineCol convierte un byte offset (típicamente
// `json.SyntaxError.Offset`) a coordenadas 1-based line/column. Cuenta
// `\n` para line, reinicia col en cada salto. Tolera offsets fuera de
// rango (los clampa al final del buffer) — la lib estándar a veces
// devuelve Offset = len(data) cuando el error es "EOF inesperado".
func offsetToLineCol(data []byte, off int) (line, col int) {
	if off < 0 {
		return 0, 0
	}
	if off > len(data) {
		off = len(data)
	}
	line, col = 1, 1
	for i := 0; i < off; i++ {
		if data[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col
}
