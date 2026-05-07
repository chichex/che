package wizard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveAtomic(t *testing.T) {
	home := t.TempDir()
	path, err := PathFor(home, "demo")
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}

	p := Pipeline{
		Status:      &Status{Stage: StageInfo, LastSavedAt: time.Now()},
		Name:        "demo",
		Description: "saved by test",
	}
	if err := Save(path, p); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// archivo final existe, .tmp no
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected %s.tmp to NOT exist, got err=%v", path, err)
	}

	// permisos 0600 en el archivo
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file perm: got %o want 0600", got)
	}

	// dir con perm 0700
	dir, _ := PipelinesDir(home)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir perm: got %o want 0700", got)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	home := t.TempDir()
	path, _ := PathFor(home, "round-trip")
	original := Pipeline{
		Status:      &Status{Stage: StageInfo, LastSavedAt: time.Unix(1700000000, 0).UTC()},
		Name:        "round-trip",
		Description: "hello",
	}
	if err := Save(path, original); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != "round-trip" {
		t.Errorf("Name: got %q", loaded.Name)
	}
	if loaded.Description != "hello" {
		t.Errorf("Description: got %q", loaded.Description)
	}
	if loaded.Status == nil || loaded.Status.Stage != StageInfo {
		t.Errorf("Status lost: %+v", loaded.Status)
	}
}

func TestDeleteIdempotent(t *testing.T) {
	home := t.TempDir()
	path, _ := PathFor(home, "to-delete")
	// borrar archivo inexistente: no debe ser error
	if err := Delete(path); err != nil {
		t.Errorf("Delete on missing: %v", err)
	}
	// crear, borrar, verificar
	if err := Save(path, Pipeline{Name: "to-delete"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Delete(path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file gone, err=%v", err)
	}
}

func TestExists(t *testing.T) {
	home := t.TempDir()
	path, _ := PathFor(home, "exists")
	ok, err := Exists(path)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Errorf("should not exist yet")
	}
	if err := Save(path, Pipeline{Name: "exists"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	ok, err = Exists(path)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Errorf("should exist after save")
	}
}

func TestPathForRespectsHome(t *testing.T) {
	home := t.TempDir()
	got, err := PathFor(home, "abc")
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	want := filepath.Join(home, ".che", "pipelines", "abc.yaml")
	if got != want {
		t.Errorf("PathFor = %q want %q", got, want)
	}
}

func TestSaveOverwritesExisting(t *testing.T) {
	home := t.TempDir()
	path, _ := PathFor(home, "ow")
	if err := Save(path, Pipeline{Name: "first", Description: "v1"}); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if err := Save(path, Pipeline{Name: "first", Description: "v2"}); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "description: v2") {
		t.Errorf("expected v2, got:\n%s", data)
	}
	if strings.Contains(string(data), "description: v1") {
		t.Errorf("v1 should be gone, got:\n%s", data)
	}
}
