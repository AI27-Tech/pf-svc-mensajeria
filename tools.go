//go:build tools

// Package tools fija las dependencias de las herramientas que se ejecutan con
// `go run` pero que el código del módulo no importa, para que `go mod tidy` no
// las borre. No se compila en ningún binario (build tag `tools`).
//
// arch-test (00 regla 5): el CI corre
//
//	go run github.com/AI27-Tech/pf-pkg-platform-go/tools/arch-test@<version> .
//
// que necesita las dependencias del tool resolubles desde el go.sum de este repo.
package tools

import (
	_ "github.com/AI27-Tech/pf-pkg-platform-go/tools/arch-test"
)
