package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// readPermissionDenials lee step-NN.events.jsonl post-mortem y devuelve los
// nombres de tools que claude pidio y le fueron denegadas durante el step.
// Solo aplica a CLIs que emiten stream-json (claude); para los demas el
// archivo no existe y devolvemos nil sin error.
//
// El motivo de leer post-mortem (en vez de trackear durante el stream) es
// que el dato vive en el ULTIMO event (`type: result`) — claude lo emite
// recien al cerrar la sesion, asi que esperar al final del subprocess es
// natural y mantiene la goroutine del tee simple (un solo lugar donde se
// extrae el dato semantico).
//
// Devuelve nil si:
//   - el archivo no existe (CLI != claude, o write fallo);
//   - el archivo es ilegible (no es responsabilidad de este path resolverlo);
//   - el ultimo event no es type=result o no tiene permission_denials.
//
// Errores de IO se ignoran a proposito: el chip warn es una mejora UX, no
// debe romper R4 si el archivo esta corrupto.
func readPermissionDenials(runDir string, idx int) []string {
	if runDir == "" {
		return nil
	}
	path := filepath.Join(runDir, fmt.Sprintf("step-%02d.events.jsonl", idx))
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// Buffer de 1 MiB matchea spawn.go: claude puede emitir lineas largas
	// (tool_input grande) y el scanner default trunca en 64 KiB silently.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	var lastResult string
	for scanner.Scan() {
		line := scanner.Bytes()
		// Filtro barato: solo nos interesan eventos type=result. La gran
		// mayoria de events son assistant/tool — saltarlos por substring
		// evita el unmarshal completo de cada linea.
		if !bytes.Contains(line, []byte(`"type":"result"`)) {
			continue
		}
		lastResult = string(line)
	}
	if lastResult == "" {
		return nil
	}

	var ev struct {
		PermissionDenials []struct {
			ToolName string `json:"tool_name"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal([]byte(lastResult), &ev); err != nil {
		return nil
	}
	if len(ev.PermissionDenials) == 0 {
		return nil
	}
	out := make([]string, 0, len(ev.PermissionDenials))
	for _, d := range ev.PermissionDenials {
		if d.ToolName == "" {
			continue
		}
		out = append(out, d.ToolName)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

