package dash

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestServe_PortFile verifica que Serve() crea ~/.che/dash.port con el puerto
// del listener y lo borra al terminar. Para no tocar el home real del usuario,
// sobreescribimos os.UserHomeDir via una variable de entorno HOME.
func TestServe_PortFile(t *testing.T) {
	// Usar un directorio temporal como HOME fake para no tocar ~/.che.
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Crear el directorio .che (igual que el dash lo espera).
	cheDir := filepath.Join(fakeHome, ".che")
	if err := os.MkdirAll(cheDir, 0o700); err != nil {
		t.Fatalf("mkdir .che: %v", err)
	}

	portFile := filepath.Join(cheDir, "dash.port")

	// Contexto cancelable para detener el server.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, Options{
			Port:   0, // puerto efimero
			NoOpen: true,
			Stdout: nil,
		})
	}()

	// Esperar a que el port file aparezca (hasta 2 segundos).
	portStr := ""
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(portFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			portStr = strings.TrimSpace(string(data))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if portStr == "" {
		t.Fatal("dash.port no apareció dentro de 2s después de Serve()")
	}

	// Verificar que el port file contiene un numero de puerto valido.
	portNum, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("dash.port contiene valor no-numerico: %q", portStr)
	}
	if portNum <= 0 || portNum > 65535 {
		t.Fatalf("dash.port fuera de rango: %d", portNum)
	}

	// Verificar que el server esta realmente escuchando en ese puerto.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", portNum), 500*time.Millisecond)
	if err != nil {
		t.Errorf("no se pudo conectar al dash en puerto %d: %v", portNum, err)
	} else {
		conn.Close()
	}

	// Cancelar el context y esperar a que Serve() retorne.
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve() retorno error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() no retorno dentro de 5s tras cancelar el context")
	}

	// Verificar que el port file fue borrado por el defer os.Remove.
	if _, err := os.Stat(portFile); !os.IsNotExist(err) {
		t.Errorf("dash.port sigue existiendo despues de que Serve() retorno: err=%v", err)
	}
}
