package explore

// deprecatedAwaitingHuman es el literal del label que representaba
// "esperando input humano" en el flow viejo. La constante vive acá
// temporalmente para que el código legacy de resume/pause compile
// mientras PR #3 lo elimina. TODO(PR#3): borrar junto con runResume,
// pauseForHuman, ListAwaiting y demás.
const deprecatedAwaitingHuman = "status:awaiting-human"
