package e2e_test

import "github.com/chichex/che/e2e/harness"

// scriptCheLockDefault agrega matchers catch-all para las tres llamadas de
// gh que hace el mecanismo che:locked: el label ensure, el POST del Lock, y
// el DELETE del Unlock. Son matchers no-consumibles (se quedan en el script
// y matchean cuantas veces haga falta) — cada test que ejerce el happy path
// de un flow dispara al menos una pareja Lock+Unlock, y todos los tests
// tienen que tolerarlas silenciosamente.
//
// Los tests que quieran asertar sobre el argumento exacto del Lock/Unlock
// pueden sobreescribir con matchers más específicos ANTES de llamar a este
// helper — el harness matchea en orden de registro, así que un matcher
// específico tiene precedencia sobre estos catch-all.
func scriptCheLockDefault(env *harness.Env) {
	env.ExpectGh(`^label create che:locked`).RespondStdout("", 0)
	env.ExpectGh(`^api -X POST repos/\{owner\}/\{repo\}/issues/\d+/labels`).RespondStdout("{}\n", 0)
	env.ExpectGh(`^api -X DELETE repos/\{owner\}/\{repo\}/issues/\d+/labels/che:`).RespondStdout("", 0)
}
