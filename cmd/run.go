package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chichex/che/internal/runner"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <slug> [prompt]",
	Short: "arranca un pipeline y streamea los logs al terminal",
	Long: `run arranca un pipeline por slug y streamea las lineas de output
de cada step al stdout con el formato [step.name] <linea>.

El backend se elige automaticamente:
  - Si el dash esta corriendo (~/.che/dash.port), usa HTTP + SSE.
  - Si no, corre el pipeline in-process (headless).

El proceso termina con exit 0 si el run termina con status=done, 1 si no.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		slug := args[0]
		var inputValue string
		if len(args) >= 2 {
			inputValue = args[1]
		} else if !isatty.IsTerminal(os.Stdin.Fd()) {
			// stdin pipeado — leer todo
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("leer stdin: %w", err)
			}
			inputValue = strings.TrimRight(string(data), "\n")
		}

		// Resolver slug → target + inputKind antes de lanzar nada.
		target, inputKind, err := runner.ResolveSlug(slug)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "che run: %v\n", err)
			os.Exit(1)
			return nil
		}

		// Validar input si el pipeline lo requiere.
		if inputKind != "none" && inputKind != "" && inputValue == "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "che run: pipeline %q requiere input — pasar como argumento o via stdin\n", slug)
			os.Exit(1)
			return nil
		}

		// Intentar descubrir el dash port.
		port := readDashPort()
		if port != "" && dialDash(port) {
			ok, runErr := runViaDash(port, slug, inputValue, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if runErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "che run (via dash): %v\n", runErr)
				os.Exit(1)
				return nil
			}
			if !ok {
				os.Exit(1)
			}
			return nil
		}

		// Fallback: headless in-process.
		ok, runErr := runViaHeadless(target, inputValue, cmd.ErrOrStderr())
		if runErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "che run (headless): %v\n", runErr)
			os.Exit(1)
			return nil
		}
		if !ok {
			os.Exit(1)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
}

// readDashPort lee ~/.che/dash.port y devuelve el puerto como string.
// Devuelve "" si el archivo no existe o no se puede leer.
func readDashPort() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".che", "dash.port"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// dialDash intenta conectarse al dash con un timeout corto. Devuelve true
// si el dash responde, false si no (en cuyo caso se cae a headless).
func dialDash(port string) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// createRunResponse es el shape del 201 de POST /api/pipelines/:slug/runs.
type createRunResponse struct {
	RunID string `json:"run_id"`
}

// runViaDash envia el run al dash via HTTP y consume el SSE stream.
// Devuelve (true, nil) si el run termino con status=done; (false, nil) si
// termino con otro estado terminal; (_, err) si hay error de transporte.
func runViaDash(port, slug, input string, stdout, stderr io.Writer) (bool, error) {
	// 1. POST /api/pipelines/<slug>/runs
	body := map[string]string{"input": input}
	bodyBytes, _ := json.Marshal(body)
	url := fmt.Sprintf("http://127.0.0.1:%s/api/pipelines/%s/runs", port, slug)
	resp, err := http.Post(url, "application/json", bytes.NewReader(bodyBytes)) //nolint:noctx
	if err != nil {
		return false, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		errBody, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var cr createRunResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return false, fmt.Errorf("parse run response: %w", err)
	}

	// 2. SSE GET /api/pipelines/<slug>/runs/<runID>/events
	return consumeSSE(port, slug, cr.RunID, stdout, stderr)
}

// consumeSSE abre el SSE stream del run y parsea eventos hasta obtener un
// run:status terminal. Devuelve (true, nil) si el status final es "done".
func consumeSSE(port, slug, runID string, stdout, stderr io.Writer) (bool, error) {
	sseURL := fmt.Sprintf("http://127.0.0.1:%s/api/pipelines/%s/runs/%s/events", port, slug, runID)
	req, err := http.NewRequest(http.MethodGet, sseURL, nil) //nolint:noctx
	if err != nil {
		return false, fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{}
	sseResp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("SSE connect %s: %w", sseURL, err)
	}
	defer sseResp.Body.Close()

	if sseResp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("SSE %s: status %d", sseURL, sseResp.StatusCode)
	}

	// stepNames mapea idx (1-based) → nombre del step, construido con step:start.
	stepNames := make(map[int]string)
	finalStatus := ""

	sc := bufio.NewScanner(sseResp.Body)
	var eventType, dataLine string

	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLine = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			// Fin del evento SSE — procesar.
			if eventType != "" && dataLine != "" {
				if err := handleSSEEvent(eventType, dataLine, stepNames, stdout, stderr); err == nil {
					// Revisar si es terminal.
					if eventType == "run:status" {
						var p map[string]any
						if json.Unmarshal([]byte(dataLine), &p) == nil {
							if s, _ := p["status"].(string); isTerminalRunStatus(s) {
								finalStatus = s
							}
						}
					}
				}
			}
			eventType = ""
			dataLine = ""
			if finalStatus != "" {
				break
			}
		case strings.HasPrefix(line, ":"):
			// heartbeat u otro comentario SSE — ignorar.
		}
		if finalStatus != "" {
			break
		}
	}

	if err := sc.Err(); err != nil && finalStatus == "" {
		return false, fmt.Errorf("SSE stream error: %w", err)
	}

	return finalStatus == "done", nil
}

// handleSSEEvent procesa un evento SSE individual.
func handleSSEEvent(eventType, dataLine string, stepNames map[int]string, stdout, stderr io.Writer) error {
	var payload map[string]any
	if err := json.Unmarshal([]byte(dataLine), &payload); err != nil {
		return err
	}

	switch eventType {
	case "step:start":
		idx := jsonInt(payload, "idx")
		name, _ := payload["name"].(string)
		if name == "" {
			name = fmt.Sprintf("step-%02d", idx)
		}
		stepNames[idx] = name

	case "step:stdout":
		idx := jsonInt(payload, "idx")
		lineTxt, _ := payload["line"].(string)
		name := stepName(stepNames, idx)
		fmt.Fprintf(stdout, "[%s] %s\n", name, lineTxt)

	case "step:end":
		idx := jsonInt(payload, "idx")
		status, _ := payload["status"].(string)
		if status == "failed" {
			exitCode := jsonInt(payload, "exit_code")
			errMsg, _ := payload["error"].(string)
			name := stepName(stepNames, idx)
			fmt.Fprintf(stderr, "[%s] FAILED exit=%d error=%s\n", name, exitCode, errMsg)
		}
	}
	return nil
}

// runViaHeadless arranca el pipeline in-process con LiveOutput=stdout.
// Devuelve (true, nil) si el run termina con status=done (Execute() retorna nil).
func runViaHeadless(target, input string, errOut io.Writer) (bool, error) {
	h, err := runner.StartHeadless(target, input, "")
	if err != nil {
		return false, err
	}
	h.LiveOutput = os.Stdout
	if execErr := h.Execute(); execErr != nil {
		fmt.Fprintf(errOut, "%v\n", execErr)
		return false, nil
	}
	return true, nil
}

// stepName devuelve el nombre del step dado su idx 1-based. Fallback a
// "step-NN" si todavia no recibimos el step:start para ese idx.
func stepName(names map[int]string, idx int) string {
	if n, ok := names[idx]; ok && n != "" {
		return n
	}
	return fmt.Sprintf("step-%02d", idx)
}

// jsonInt extrae un int de un map[string]any donde el valor puede ser
// float64 (JSON number) o int.
func jsonInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

// isTerminalRunStatus devuelve true para los status que indican que el run
// termino. Misma logica que isTerminalStatus en internal/dash/watcher.go.
func isTerminalRunStatus(status string) bool {
	switch status {
	case "done", "failed", "interrupted", "cancelled":
		return true
	}
	return false
}
