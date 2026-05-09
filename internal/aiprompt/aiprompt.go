// Package aiprompt arma el prompt que el usuario va a pegar en su cliente
// de IA preferido (claude.ai, ChatGPT, etc) para que le genere un pipeline
// YAML compatible con `che`. El prompt encapsula:
//
//   - el schema literal de pipeline / step / validator,
//   - los enums permitidos (cli, kind, input, on_max_loops),
//   - reglas de "prompt sano" (imperativo, no interactivo, sin AskUser-
//     Question) — mismas lecciones que aprendimos en la review automatica
//     de S2 (internal/wizard/promptreview),
//   - 1 ejemplo concreto para anclar el shape esperado.
//
// El output del paquete es un string listo para copiar al portapapeles —
// sin templating de ejecucion (no llama a ningun CLI).
package aiprompt

import (
	"fmt"
	"strings"
)

// Build devuelve el prompt completo a copiar. name + description son los 2
// inputs que el usuario tipea en la pantalla nueva. Ambos pueden venir con
// espacios al frente/atras; los limpiamos antes de embedder.
func Build(name, description string) string {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if name == "" {
		name = "<sin nombre>"
	}
	if description == "" {
		description = "<sin descripcion>"
	}
	return fmt.Sprintf(template, name, description)
}

// template es el cuerpo del prompt. Dos %s: name + description.
//
// Decisiones de diseno:
//   - Pedimos JSON-style "SOLO el YAML" para que el usuario pueda copiar/
//     pegar sin tener que limpiar code fences.
//   - Listamos los CLIs disponibles. claude default porque el harness de
//     che lo trata como ciudadano de primera (stream-json + parser).
//   - Imperativo + no-interactivo es la regla #1 — el modo de falla mas
//     comun (claude pidiendo AskUserQuestion en no-TTY → exit 0 sin hacer
//     nada) viene de prompts analiticos, no imperativos.
//   - Validator se ofrece OPCIONAL — la decision de incluirlo depende de
//     si el step lo amerita (ej. "el output tiene que matchear formato X").
const template = `Necesito que evalues si lo siguiente tiene sentido como pipeline de ` + "`che`" + ` (un CLI orquestador de agentes de IA) y, solo si es viable, generes el YAML correspondiente.

## Pipeline candidato

Nombre: %s
Objetivo: %s

## Paso 1 — Analisis de viabilidad (OBLIGATORIO)

Antes de tirar YAML, evaluá honestamente si este objetivo tiene sentido como pipeline automatizado de ` + "`che`" + `. Un pipeline es buena fit cuando:

- El objetivo es **repetitivo** (lo vas a correr mas de una vez, no es one-shot).
- Tiene **pasos discretos y secuenciales** que se pueden chainear (output del step N alimenta al N+1).
- Cada paso es **automatizable end-to-end** (ejecutable sin necesidad de juicio humano en mitad de cada step).
- El estado final esperado es **deterministico y verificable** (sabes cuando esta "hecho").
- Una alternativa mas simple (un solo prompt a un asistente, un alias bash, un script) **NO** alcanza.

Es **mala** fit cuando:

- Es un task one-shot exploratorio sin valor de re-corrida.
- Necesita confirmacion humana en cada step (el runner es no-TTY — no podes pedir input).
- Es trivial: un solo prompt o un script bash de 5 lineas haria lo mismo.
- Es ambiguo: el "estado final" depende del juicio del momento, no hay criterio objetivo de exito.

Devolveme tu analisis con este formato exacto al inicio de tu respuesta:

` + "```" + `
VIABILIDAD: <recomendado | overkill | no-recomendado | dudoso>
RAZONAMIENTO: <2-4 frases concretas — por que cae en ese verdict, citando 1-2 criterios de arriba>
RECOMENDACION: <que hacer en su lugar — si recomendado: "proceder con el YAML abajo"; si overkill: "alcanza con un alias / script — ejemplo"; si no-recomendado: "este flow no se beneficia de pipeline; probar X"; si dudoso: "podes intentarlo, pero considera Y antes">
` + "```" + `

Si el verdict es ` + "`no-recomendado`" + `: PARA AHI. No generes YAML. Termina con la recomendacion.

Si el verdict es ` + "`overkill`" + `, ` + "`dudoso`" + ` o ` + "`recomendado`" + `: continua con el Paso 2. Para overkill / dudoso aclara en RECOMENDACION cual es la alternativa mas simple antes de pasar al YAML.

## Paso 2 — Schema literal del YAML

` + "```" + `yaml
name: <slug-en-kebab-case>
description: <una frase>
steps:
  - name: <nombre-del-step>
    cli: claude            # claude | codex | gemini | opencode
    kind: prompt           # prompt | skill
    content: |             # si kind=prompt: el prompt literal a ejecutar
      <prompt imperativo>  # si kind=skill: el nombre del skill (slash command)
    input: text            # text | pr | issue | file | url | none | previous_output
    validator:             # OPCIONAL — bloque de cross-review
      cli: claude
      kind: prompt
      content: |
        <prompt que verifica que el output del step cumpla el contrato>
    max_loops: 3           # solo si hay validator. 1..5
    on_max_loops: fail     # solo si hay validator. fail | continue | pause
` + "```" + `

## Reglas obligatorias

1. **Imperativo, no analitico**. Los prompts deben pedir ACCIONES concretas
   ("ejecuta gh pr merge --merge --delete-branch <PR>"), nunca analisis o
   evaluacion abierta ("analiza el PR para decidir si esta listo"). El
   pipeline va a EJECUTARSE, no a generar texto.

2. **No interactivo**. El runner corre en no-TTY. NUNCA asumas que se puede
   pedir confirmacion humana. Prohibi explicitamente AskUserQuestion en los
   prompts si el contexto lo amerita ("NO uses AskUserQuestion ni similares;
   si encontras ambiguedad, abortar con exit 1").

3. **Tools especificas mencionadas**. Si el step necesita ` + "`gh`" + `, ` + "`git`" + `, ` + "`bash`" + `,
   nombralas. Si NO debe tocar ciertos archivos, prohibirlo.

4. **Inputs encadenados**. Step 0 elige input segun el contexto (text / pr /
   issue / file / url / none). Steps 1+ usan ` + "`input: previous_output`" + `
   cuando dependen del output del step anterior — no inventes otro flow.

5. **Validator donde aplica**. Pone validator block en steps cuyos outputs
   tienen un contrato claro (formato, esquema, campos esperados). Steps
   exploratorios o read-only no necesitan validator.

6. **Slug del name**. El campo ` + "`name`" + ` raiz del pipeline tiene que ser un
   slug en kebab-case (lowercase, guiones, sin caracteres especiales).

## Ejemplo de un pipeline bien formado

` + "```" + `yaml
name: triage-checkout-flow
description: Toma una metrica anomala y dispara un triage del flow de checkout.
steps:
  - name: collect-signals
    cli: claude
    kind: prompt
    content: |
      Recibis una metrica con anomalia en stdin. Recolecta TODAS las senales
      relevantes ejecutando:
      - gh issue list --label checkout --state open --json number,title,body
      - git log --since='7 days ago' --grep=checkout --oneline
      Devolve un JSON con shape:
        {"metric": "...", "issues": [...], "commits": [...]}
      NO uses AskUserQuestion. Si no hay senales, exit 1.
    input: text
  - name: triage-decision
    cli: claude
    kind: prompt
    content: |
      Recibis el JSON del step anterior. Decidi un owner + severidad y
      ejecuta gh issue create --label triage --title <...> --body <...> con
      la sintesis. Devolve el numero del issue creado.
      NO pidas confirmacion humana.
    input: previous_output
    validator:
      cli: claude
      kind: prompt
      content: |
        Verifica que el output sea un numero de issue valido (regex ^#?\d+$)
        y que el issue exista en el repo activo (gh issue view <num>).
    max_loops: 2
    on_max_loops: fail
` + "```" + `

## Output esperado

Tu respuesta tiene 2 partes en este orden:

1. **Bloque VIABILIDAD** (siempre, en el formato literal del Paso 1, dentro de un bloque ` + "```" + `text fence).
2. **YAML del pipeline** (solo si VIABILIDAD ≠ no-recomendado): sin code fences, sin comentarios extra, sin texto antes ni despues. Empezar la primera linea con ` + "`name:`" + ` y terminar con la ultima clave del ultimo step.

NO mezcles los dos bloques. NO devuelvas YAML si recomendaste no hacer el pipeline.
`
