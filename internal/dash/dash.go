// Package dash sirve el dashboard local de che como HTML estatico.
//
// El subcomando `che dash` levanta un HTTP server en 127.0.0.1 que
// devuelve el prototipo standalone embebido. El prototipo trae datos
// mockeados y simula SSE en JS — wire al runner real es futuro.
package dash

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

//go:embed assets/dash.html
var assets embed.FS

// Options configura como arranca el server.
type Options struct {
	// Port es el TCP port preferido. 0 => puerto efimero.
	Port int
	// NoOpen suprime el intento de abrir el browser.
	NoOpen bool
	// Stdout recibe los mensajes de status. Si nil, se descartan.
	Stdout io.Writer
}

// Serve arranca el server y bloquea hasta que ctx se cancele.
//
// El bind es a 127.0.0.1 — no exponer en red. Si Port esta ocupado,
// cae a un puerto efimero asignado por el SO.
func Serve(ctx context.Context, opts Options) error {
	out := opts.Stdout
	if out == nil {
		out = io.Discard
	}

	ln, err := listen(opts.Port)
	if err != nil {
		return err
	}

	// Escribir ~/.che/dash.port con el TCP port del listener para que
	// `che run` pueda descubrir el dash sin configuracion. Si falla,
	// logear y seguir — el dash debe arrancar igual aunque `che run`
	// no pueda descubrirlo y caiga a headless.
	portFile := ""
	if home, herr := os.UserHomeDir(); herr == nil {
		portFile = filepath.Join(home, ".che", "dash.port")
		addr := ln.Addr().(*net.TCPAddr)
		if werr := os.WriteFile(portFile, []byte(fmt.Sprintf("%d", addr.Port)), 0o600); werr != nil {
			fmt.Fprintf(out, "[dash] no se pudo escribir dash.port: %v\n", werr)
			portFile = "" // no intentar borrar lo que no se creo
		}
	}
	if portFile != "" {
		defer os.Remove(portFile)
	}

	// Resolve pipelines and runs directories. On failure use "" so handlers
	// return empty list / 404 instead of crashing.
	pipelinesDir := ""
	runsDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		pipelinesDir = filepath.Join(home, ".che", "pipelines")
		runsDir = filepath.Join(home, ".che", "runs")
	}

	// Singleton bus for SSE per-run streaming.
	bus := NewBus(runsDir)

	// Start global watchers (always active, not ref-counted).
	// They stop when ctx is cancelled (via stopCh signalled in shutdown goroutine).
	pw := newPipelinesWatcher(pipelinesDir, bus)
	rw := newRunsWatcher(runsDir, bus)
	go pw.run()
	go rw.run()
	// Stop global watchers when context is cancelled.
	go func() {
		<-ctx.Done()
		pw.stop()
		rw.stop()
	}()

	// Run starter + lock — comparte estado entre handlers para que el POST
	// /runs no pueda double-arrancar el mismo slug. runsRoot="" deja al
	// runner usar ~/.che/runs (mismo path que la TUI).
	starter := &runnerStarter{runsRoot: ""}
	lock := newRunLock()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/events", handleGlobalEvents(bus))
	mux.HandleFunc("/api/pipelines", handleListPipelines(pipelinesDir, runsDir))
	mux.HandleFunc("/api/pipelines/", dispatchPipelinesPrefix(pipelinesDir, runsDir, bus, starter, lock))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	fmt.Fprintf(out, "che dash listening on %s\n", url)
	fmt.Fprintln(out, "press ctrl+c to stop")

	if !opts.NoOpen {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(out, "could not open browser: %v\n", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// listen intenta el puerto solicitado primero; si esta ocupado o es 0,
// usa uno efimero. Asi `che dash` no falla si el usuario ya tiene 7878.
func listen(port int) (net.Listener, error) {
	if port != 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(assets, "assets/dash.html")
	if err != nil {
		http.Error(w, "dash asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
