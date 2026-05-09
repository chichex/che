// Package repoctx detecta si el cwd del proceso che esta parado dentro de
// un repo de github (segun lo que sabe `gh`). El resultado lo consumen tres
// surfaces:
//
//   - el wizard, para deshabilitar las pills `pr` / `issue` cuando el cwd
//     no soporta esos kinds (no tiene sentido ofrecer "elegi un PR del
//     repo" si no hay repo);
//   - el lister "My pipelines", para chipear con `[needs repo]` los
//     pipelines que asumen un repo (algun step pide pr/issue);
//   - el runner R1 (picker de PRs/issues abiertos) y R2 (preflight
//     "git repo context").
//
// La deteccion es estable durante la vida del proceso (cd dentro de la TUI
// no es algo que ofrezcamos), asi que la cacheamos en el primer Detect().
// Para tests, defaultDetect es swappable via SetDetectFn.
package repoctx

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Info es el resultado de la deteccion. InGitHubRepo=true significa que
// `gh repo view` exit 0 sobre el cwd; Repo es lo que devolvio
// `nameWithOwner` (formato "owner/name"). Si InGitHubRepo=false, Repo
// queda vacio.
type Info struct {
	InGitHubRepo bool
	Repo         string
}

// detectFn es el bajo-nivel; tests lo reemplazan via SetDetectFn. El
// default ejecuta `gh repo view --json nameWithOwner -q .nameWithOwner`
// con timeout corto.
var (
	detectMu  sync.Mutex
	detectFn  = defaultDetect
	cached    *Info
	cachedSet bool
)

// detectTimeout acota cuanto puede tardar la primera invocacion. gh local
// suele resolver en <500ms; si esta colgado preferimos asumir "no repo"
// antes que freezear el wizard al abrir.
const detectTimeout = 3 * time.Second

// Detect devuelve el contexto del repo segun el cwd. Cachea el resultado
// para no spawn-ear gh mas de una vez por proceso. En tests, ResetForTest
// limpia el cache; SetDetectFn instala un fake.
func Detect() Info {
	detectMu.Lock()
	defer detectMu.Unlock()
	if cachedSet {
		return *cached
	}
	info := detectFn()
	cached = &info
	cachedSet = true
	return info
}

// SetDetectFn instala un fake (tests). Reset implicito del cache para que
// la primera Detect() post-set ejecute el fake. Devuelve la fn anterior
// para que los tests puedan restaurar via t.Cleanup.
func SetDetectFn(fn func() Info) func() Info {
	detectMu.Lock()
	defer detectMu.Unlock()
	prev := detectFn
	detectFn = fn
	cached = nil
	cachedSet = false
	return prev
}

// ResetForTest limpia el cache sin tocar la fn instalada. Util cuando el
// test cambia el cwd entre subcasos y quiere forzar una re-deteccion.
func ResetForTest() {
	detectMu.Lock()
	defer detectMu.Unlock()
	cached = nil
	cachedSet = false
}

func defaultDetect() Info {
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	out, err := cmd.Output()
	if err != nil {
		return Info{}
	}
	repo := strings.TrimSpace(string(out))
	if repo == "" {
		return Info{}
	}
	return Info{InGitHubRepo: true, Repo: repo}
}
