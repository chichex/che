//go:build windows

package runner

import "os/exec"

// setProcAttrs es no-op en windows: no tenemos pgids en el sentido POSIX y
// los e2e + el flow real de che corren solo en darwin/linux. Lo dejamos
// declarado para que el archivo spawn.go compile en cross-build.
func setProcAttrs(_ *exec.Cmd) {}
