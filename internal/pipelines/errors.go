package pipelines

import "errors"

// errEmptyCwd lo devuelve PathForScope/ProjectPipelinesDir cuando se pide
// scope project sin un cwd resuelto. El caller decide si surfacearlo o
// degradar a scope global.
var errEmptyCwd = errors.New("pipelines: cwd vacio para scope project")
