package execute

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

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

	// Worktree no existe. Primero fetch de la base para tener la referencia
	// actualizada. Si hay remote `origin`, actualizamos; si no, no rompemos
	// (puede ser un repo local sin remote — caso de tests).
	_ = runGit(opts.RepoRoot, "fetch", "origin", base)

	branchExists, err := branchExists(opts.RepoRoot, branch)
	if err != nil {
		return nil, err
	}

	if branchExists {
		// Reuse branch (caso: re-ejecutar después de borrar worktree a mano).
		if err := runGit(opts.RepoRoot, "worktree", "add", path, branch); err != nil {
			return nil, fmt.Errorf("git worktree add (existing branch): %w", err)
		}
	} else {
		if err := runGit(opts.RepoRoot, "worktree", "add", "-b", branch, path, base); err != nil {
			return nil, fmt.Errorf("git worktree add: %w", err)
		}
	}

	return &Worktree{Path: path, Branch: branch}, nil
}

// Cleanup elimina el worktree y (si keepBranch=false) borra la branch local.
// Idempotente: llamar dos veces no es error.
func (w *Worktree) Cleanup(repoRoot string, keepBranch bool) error {
	if w == nil || w.cleaned {
		return nil
	}
	w.cleaned = true

	// Intentamos remove; si falla (worktree ya no existía), no reportamos
	// error fatal — el objetivo es dejar todo limpio.
	_ = runGit(repoRoot, "worktree", "remove", "--force", w.Path)

	if !keepBranch {
		_ = runGit(repoRoot, "branch", "-D", w.Branch)
	}
	return nil
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
