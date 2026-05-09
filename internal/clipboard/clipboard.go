// Package clipboard copia texto al portapapeles del sistema sin agregar
// dependencias C/cgo. Detecta el binario apropiado en runtime: pbcopy en
// macOS, wl-copy o xclip en Linux, clip.exe en Windows. Si ninguno esta
// disponible devuelve un error claro para que el caller pueda surfar el
// fallback ("copia manual").
package clipboard

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// ErrNoCopier indica que no se pudo encontrar un binario de portapapeles
// soportado en el PATH del proceso.
var ErrNoCopier = errors.New("no clipboard copier disponible (probar pbcopy / wl-copy / xclip)")

// Copy escribe s al portapapeles del sistema. Bloquea hasta que el binario
// del copier termina. No emite output a stdout/stderr — los errores van por
// el return.
func Copy(s string) error {
	cmd, err := commandForOS()
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// commandForOS arma el *exec.Cmd apropiado para la plataforma actual,
// preferendo el copier nativo. Devuelve ErrNoCopier si ninguno aplica.
func commandForOS() (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("pbcopy"); err == nil {
			return exec.Command(path), nil
		}
	case "linux":
		// Wayland primero (mas comun en distros nuevas), X11 como fallback.
		if path, err := exec.LookPath("wl-copy"); err == nil {
			return exec.Command(path), nil
		}
		if path, err := exec.LookPath("xclip"); err == nil {
			return exec.Command(path, "-selection", "clipboard"), nil
		}
		if path, err := exec.LookPath("xsel"); err == nil {
			return exec.Command(path, "--clipboard", "--input"), nil
		}
	case "windows":
		if path, err := exec.LookPath("clip.exe"); err == nil {
			return exec.Command(path), nil
		}
	}
	return nil, ErrNoCopier
}
