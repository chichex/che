package aiprompt

import (
	"strings"
	"testing"
)

func TestBuild_EmbedsNameAndDescription(t *testing.T) {
	out := Build("triage-flow", "dispara un triage cuando hay anomalia")
	if !strings.Contains(out, "Nombre: triage-flow") {
		t.Errorf("expected name in prompt:\n%s", out)
	}
	if !strings.Contains(out, "Objetivo: dispara un triage cuando hay anomalia") {
		t.Errorf("expected description in prompt:\n%s", out)
	}
}

func TestBuild_BlankFallback(t *testing.T) {
	out := Build("", "  ")
	if !strings.Contains(out, "Nombre: <sin nombre>") {
		t.Errorf("expected blank-name fallback")
	}
	if !strings.Contains(out, "Objetivo: <sin descripcion>") {
		t.Errorf("expected blank-desc fallback")
	}
}

func TestBuild_ContainsRules(t *testing.T) {
	out := Build("x", "y")
	for _, want := range []string{
		"Imperativo",
		"No interactivo",
		"AskUserQuestion",
		"previous_output",
		"validator",
		"sin code fences",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in prompt", want)
		}
	}
}

func TestBuild_ContainsViabilityAnalysis(t *testing.T) {
	out := Build("x", "y")
	// El analisis de viabilidad es OBLIGATORIO en la respuesta de la IA.
	// Aseguramos que el prompt lo pida explicito + el formato sea literal
	// (cualquier downstream que parsee la respuesta busca estos sentinels).
	for _, want := range []string{
		"VIABILIDAD:",
		"RAZONAMIENTO:",
		"RECOMENDACION:",
		"recomendado",
		"overkill",
		"no-recomendado",
		"dudoso",
		"PARA AHI",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in prompt", want)
		}
	}
}
