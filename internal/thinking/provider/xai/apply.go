package xai

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/codex"
)

type Applier struct {
	codex.Applier
}

var _ thinking.ProviderApplier = (*Applier)(nil)

func newApplier() *Applier {
	return &Applier{}
}

func init() {
	thinking.RegisterProvider("xai", newApplier())
}
