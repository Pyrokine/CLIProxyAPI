package xai

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/codex"
)

type applier struct {
	codex.Applier
}

var _ thinking.ProviderApplier = (*applier)(nil)

func newApplier() *applier {
	return &applier{}
}

func init() {
	thinking.RegisterProvider("xai", newApplier())
}
