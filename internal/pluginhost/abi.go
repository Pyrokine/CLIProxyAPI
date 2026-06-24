package pluginhost

import (
	"context"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
)

const pluginHostABIVersion = pluginabi.ABIVersion

type pluginClient interface {
	Call(ctx context.Context, method string, request []byte) ([]byte, error)
	Shutdown()
}

type pluginLoader interface {
	Open(file pluginFile, host *Host) (pluginClient, error)
}
