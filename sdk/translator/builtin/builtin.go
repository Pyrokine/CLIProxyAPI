// Package builtin exposes the built-in translator registrations for SDK users.
package builtin

import (
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"

	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
)

// Registry exposes the default registry populated with all built-in translators.
// noinspection GoUnusedExportedFunction
func Registry() *sdktranslator.Registry {
	return sdktranslator.Default()
}

// Pipeline returns a pipeline that already contains the built-in translators.
// noinspection GoUnusedExportedFunction
func Pipeline() *sdktranslator.Pipeline {
	return sdktranslator.NewPipeline(sdktranslator.Default())
}
