package execute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// DetectBaseBranch devuelve el default branch del repo remoto. Preferimos
// `gh repo view --json defaultBranchRef` (autoritativo sobre GitHub); si gh
// falla o el repo no tiene remoto configurado en gh, caemos a
// `git symbolic-ref refs/remotes/origin/HEAD` (el HEAD local del remote, que
// suele quedar seteado al clonar). Si ambos fallan devolvemos error — el
// caller decide si quiere fallar o asumir "main".
func DetectBaseBranch(ctx context.Context) (string, error) {
	if v := strings.TrimSpace(os.Getenv("CHE_BASE_BRANCH")); v != "" {
		return v, nil
	}

	var ghErr error
	out, err := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "defaultBranchRef").Output()
	if err == nil {
		var parsed struct {
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
		}
		if jerr := json.Unmarshal(out, &parsed); jerr == nil {
			if name := strings.TrimSpace(parsed.DefaultBranchRef.Name); name != "" {
				return name, nil
			}
			ghErr = fmt.Errorf("gh repo view: defaultBranchRef vacío en la respuesta")
		} else {
			ghErr = fmt.Errorf("gh repo view: parsing JSON: %w", jerr)
		}
	} else {
		if ee, ok := err.(*exec.ExitError); ok {
			ghErr = fmt.Errorf("gh repo view: %s", strings.TrimSpace(string(ee.Stderr)))
		} else {
			ghErr = fmt.Errorf("gh repo view: %w", err)
		}
	}

	out2, err2 := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err2 == nil {
		name := strings.TrimSpace(string(out2))
		name = strings.TrimPrefix(name, "origin/")
		if name != "" {
			return name, nil
		}
	}

	return "", fmt.Errorf("no se pudo detectar el default branch: %v; fallback git symbolic-ref también falló: %v", ghErr, err2)
}

// Worktree representa un git worktree aislado creado por `che execute` para
// trabajar un issue sin ensuciar el cwd del usuario. Contiene la ruta
// absoluta del worktree, el nombre de la branch creada y un closure de
// cleanup para liberarlo.
type Worktree struct {
	Path    string
	Branch  string
	cleaned bool
}

// WorktreeOpts controla la creación del worktree. RepoRoot es la raíz del
// repositorio donde va a vivir el directorio .worktrees (típicamente el cwd
// del usuario detectado por `git rev-parse --show-toplevel`). BaseBranch es
// la branch desde la que se parte (default: main). Si IssueNum o Slug están
// vacíos, devuelve error — la combinación es parte de la convención.
type WorktreeOpts struct {
	RepoRoot   string
	IssueNum   int
	Slug       string
	BaseBranch string // default: "main"
}

// slugRe colapsa caracteres no ASCII/no alfanuméricos a "-" para producir
// nombres de branch/path válidos y cortos.
var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify convierte un título libre a un slug apto para nombres de branch y
// paths. Lowercase + colapsa todo lo no alfanumérico a "-", recorta hyphens
// al principio/final y limita a 40 chars para no desbordar nombres de
// branches en GitHub.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		return "issue"
	}
	return s
}

// CreateWorktree crea (o reutiliza) un worktree aislado en
// <repoRoot>/.worktrees/issue-<N> sobre la branch exec/<N>-<slug>. Si el
// worktree ya existe apuntando a la misma branch, lo reutiliza — eso
// habilita re-ejecuciones idempotentes. Si la branch existe pero en otro
// lado, devuelve error para evitar apropiarse de algo del usuario.
//
// Flujo:
//  1. Chequea que RepoRoot sea el toplevel de un repo git.
//  2. Mira si el worktree ya existe en la ruta esperada; si sí y apunta a la
//     branch esperada, reutilizar.
//  3. Si la branch existe pero el worktree no, `git worktree add` reusando
//     la branch.
//  4. Si ni branch ni worktree existen, `git worktree add -b <branch>
//     <path> <base>` (crea branch desde base actualizada).
func CreateWorktree(opts WorktreeOpts) (*Worktree, error) {
	if opts.IssueNum <= 0 {
		return nil, fmt.Errorf("worktree: issue number must be > 0")
	}
	if strings.TrimSpace(opts.Slug) == "" {
		return nil, fmt.Errorf("worktree: slug is required")
	}
	if strings.TrimSpace(opts.RepoRoot) == "" {
		return nil, fmt.Errorf("worktree: repo root is required")
	}
	base := opts.BaseBranch
	if base == "" {
		base = "main"
	}

	path := filepath.Join(opts.RepoRoot, ".worktrees", fmt.Sprintf("issue-%d", opts.IssueNum))
	branch := fmt.Sprintf("exec/%d-%s", opts.IssueNum, opts.Slug)

	existing, err := worktreeAt(opts.RepoRoot, path)
	if err != nil {
		return nil, err
	}
	if existing != "" {
		if existing != branch {
			return nil, fmt.Errorf("worktree %s already exists on branch %q, expected %q — remove manually with `git worktree remove %s`", path, existing, branch, path)
		}
		return &Worktree{Path: path, Branch: branch}, nil
	}

	// Worktree no existe. Fetch obligatorio de la base desde origin para no
	// partir de un main local stale (si la branch nueva se crea a partir de
	// un main viejo, el PR queda con commits obsoletos). Override con
	// CHE_EXEC_SKIP_FETCH=1 para tests e2e con bare remotes locales.
	skipFetch := os.Getenv("CHE_EXEC_SKIP_FETCH") == "1"
	if !skipFetch {
		if err := runGit(opts.RepoRoot, "fetch", "origin", base); err != nil {
			return nil, fmt.Errorf("git fetch origin %s: %w — para tests con bare remotes locales setear CHE_EXEC_SKIP_FETCH=1", base, err)
		}
	}

	branchExists, err := branchExists(opts.RepoRoot, branch)
	if err != nil {
		return nil, err
	}

	// Ref desde la que partir para nuevas branches: preferimos origin/<base>
	// (el estado real del remote tras el fetch) antes que el <base> local,
	// que puede estar stale. Si no hay origin/<base> (repo sin ese ref),
	// fallback al local.
	startRef := "origin/" + base
	hasRemoteBase, refErr := hasRef(opts.RepoRoot, startRef)
	if refErr != nil {
		return nil, refErr
	}
	if !hasRemoteBase {
		// Si el usuario pidió saltar el fetch (tests), no emitimos el
		// warning — es esperable que origin/<base> no exista.
		if !skipFetch {
			fmt.Fprintf(os.Stderr, "warning: %s no existe, cayendo a %s local (puede estar stale)\n", startRef, base)
		}
		startRef = base
	}

	if branchExists {
		// Reuse branch (caso: re-ejecutar después de borrar worktree a mano).
		if err := runGit(opts.RepoRoot, "worktree", "add", path, branch); err != nil {
			return nil, fmt.Errorf("git worktree add (existing branch): %w", err)
		}
	} else {
		if err := runGit(opts.RepoRoot, "worktree", "add", "-b", branch, path, startRef); err != nil {
			return nil, fmt.Errorf("git worktree add: %w", err)
		}
	}

	return &Worktree{Path: path, Branch: branch}, nil
}

// hasRef devuelve true si la referencia git existe en el repo.
func hasRef(repoRoot, ref string) (bool, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

// Cleanup elimina el worktree y (si keepBranch=false) borra la branch local.
// Acepta un ctx para acotar cada operación y devuelve (joined) los errores
// que haya — el caller es responsable de loguearlos. Es idempotente:
// llamar dos veces no es error (la segunda es no-op). "Ya no existía" no
// se considera error para no ensuciar el log cuando alguien limpió a mano.
func (w *Worktree) Cleanup(ctx context.Context, repoRoot string, keepBranch bool) error {
	if w == nil || w.cleaned {
		return nil
	}
	w.cleaned = true

	var errs []error
	if err := runGitCtx(ctx, repoRoot, "worktree", "remove", "--force", w.Path); err != nil {
		if !isMissingWorktreeErr(err) {
			errs = append(errs, fmt.Errorf("worktree remove: %w", err))
		}
	}
	if !keepBranch {
		if err := runGitCtx(ctx, repoRoot, "branch", "-D", w.Branch); err != nil {
			if !isMissingBranchErr(err) {
				errs = append(errs, fmt.Errorf("branch -D: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// isMissingWorktreeErr detecta el caso en que `git worktree remove` falla
// porque el worktree ya no estaba registrado. No lo consideramos un error
// real para no ensuciar el log cuando algo lo limpió antes (por ejemplo,
// el test que corre Cleanup en un defer).
func isMissingWorktreeErr(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "is not a working tree") ||
		strings.Contains(m, "not a valid working tree") ||
		strings.Contains(m, "No such file or directory")
}

// isMissingBranchErr detecta el caso en que `git branch -D` falla porque
// la branch ya no existía (la borraron a mano o nunca se creó).
func isMissingBranchErr(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "not found") || strings.Contains(m, "No such branch")
}

// worktreeAt devuelve la branch actualmente checkouteada en un path si hay
// un worktree registrado ahí, o "" si no hay. Parsea la salida de
// `git worktree list --porcelain`. Normaliza paths con EvalSymlinks porque
// en macOS los TMPDIR pueden venir con prefijo /private/var vs /var y git
// imprime siempre la forma resuelta.
func worktreeAt(repoRoot, path string) (string, error) {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git worktree list: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	target := canonicalPath(path)
	var currentPath, currentBranch string
	lines := strings.Split(string(out), "\n")
	check := func() string {
		if canonicalPath(currentPath) == target {
			return currentBranch
		}
		return ""
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if b := check(); currentPath != "" && canonicalPath(currentPath) == target {
				return b, nil
			}
			currentPath, currentBranch = "", ""
			continue
		}
		if strings.HasPrefix(trimmed, "worktree ") {
			currentPath = strings.TrimPrefix(trimmed, "worktree ")
		} else if strings.HasPrefix(trimmed, "branch ") {
			currentBranch = strings.TrimPrefix(trimmed, "branch refs/heads/")
		}
	}
	if b := check(); currentPath != "" && b != "" || canonicalPath(currentPath) == target {
		return currentBranch, nil
	}
	return "", nil
}

// canonicalPath resuelve symlinks para comparar paths robustamente. Si el
// path no existe (todavía no fue creado) cae a filepath.Clean.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	// Intenta resolver el padre si el path no existe, así matcheamos cuando
	// comparamos la ruta a crear contra una ya creada con prefijo distinto.
	dir, base := filepath.Split(p)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(resolved, base)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}


// branchExists devuelve true si la referencia local existe.
func branchExists(repoRoot, branch string) (bool, error) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

// runGit corre `git -C repoRoot args...` y devuelve error con stderr si
// falla. stdout no se captura — los comandos que usamos no lo necesitan.
func runGit(repoRoot string, args ...string) error {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// runGitCtx es runGit con context — si ctx se cancela, el subproceso se mata.
// Lo usamos en los pasos de cleanup para que un `git worktree remove` colgado
// no deje al usuario esperando indefinidamente tras una señal.
func runGitCtx(ctx context.Context, repoRoot string, args ...string) error {
	full := append([]string{"-C", repoRoot}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}
