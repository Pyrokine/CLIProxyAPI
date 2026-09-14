package management

import (
	"net/http"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

const managementClientAddressContextKey = "management.client-address"

type managementClientAddress struct {
	ip        string
	local     bool
	peerLocal bool
	source    access.ClientAddressSource
	resolved  access.ClientAddress
}

func managementClientAddressFromContext(c *gin.Context) (managementClientAddress, bool) {
	if c == nil {
		return managementClientAddress{}, false
	}
	value, exists := c.Get(managementClientAddressContextKey)
	if !exists {
		return managementClientAddress{}, false
	}
	client, ok := value.(managementClientAddress)
	return client, ok
}

func resolveManagementClientAddress(r *http.Request, cfg *config.Config) managementClientAddress {
	var trustedProxies []string
	if cfg != nil {
		trustedProxies = cfg.RemoteManagement.TrustedProxies
	}
	var (
		client   access.ClientAddress
		resolved bool
	)
	if r != nil {
		client, resolved = access.ClientAddressFromContext(r.Context())
	}
	if !resolved {
		client = access.ResolveClientAddress(r, trustedProxies)
	}
	return managementClientAddress{
		ip:        client.Key,
		local:     client.DirectLoopback,
		peerLocal: client.PeerLoopback && !client.ForwardingHeadersPresent,
		source:    client.Source,
		resolved:  client,
	}
}
