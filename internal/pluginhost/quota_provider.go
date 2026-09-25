package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

// RegisteredQuotaProviderInfo describes one active plugin quota provider.
type RegisteredQuotaProviderInfo struct {
	PluginID           string   `json:"plugin_id"`
	Provider           string   `json:"provider"`
	DisplayName        string   `json:"display_name,omitempty"`
	SupportedProviders []string `json:"supported_providers,omitempty"`
	SupportsReset      bool     `json:"supports_reset"`
}

// QuotaProviderIdentifiers returns unique active quota provider identifiers.
func (h *Host) QuotaProviderIdentifiers() []string {
	if h == nil {
		return nil
	}
	seen := make(map[string]struct{})
	identifiers := make([]string, 0)
	for _, record := range h.activeRecords() {
		provider := record.plugin.Capabilities.QuotaProvider
		identifier, okIdentifier := h.callQuotaIdentifier(record, provider)
		if !okIdentifier {
			continue
		}
		if _, duplicate := seen[identifier]; duplicate {
			continue
		}
		seen[identifier] = struct{}{}
		identifiers = append(identifiers, identifier)
	}
	return identifiers
}

// QuotaProviders lists active quota providers and their declared provider coverage.
func (h *Host) QuotaProviders(ctx context.Context) []RegisteredQuotaProviderInfo {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := make([]RegisteredQuotaProviderInfo, 0)
	for _, record := range h.activeRecords() {
		if errContext := ctx.Err(); errContext != nil {
			break
		}
		provider := record.plugin.Capabilities.QuotaProvider
		if provider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
			continue
		}
		identifier, okIdentifier := h.callQuotaIdentifier(record, provider)
		if !okIdentifier {
			if h.isPluginFused(record.id) {
				continue
			}
			identifier = normalizeProviderID(record.id)
		}
		if identifier == "" {
			continue
		}
		description, handled, errDescribe := h.callQuotaDescribe(ctx, record, provider)
		if errDescribe != nil {
			log.WithError(errDescribe).WithField("plugin_id", record.id).Warn(
				"pluginhost: describe quota provider failed",
			)
			description = pluginapi.QuotaDescribeResponse{}
		} else if !handled {
			description = pluginapi.QuotaDescribeResponse{}
		}
		supported := normalizedQuotaProviders(description.SupportedProviders)
		if len(supported) == 0 {
			supported = []string{identifier}
		}
		displayName := strings.TrimSpace(description.DisplayName)
		if displayName == "" {
			displayName = strings.TrimSpace(record.meta.Name)
		}
		if displayName == "" {
			displayName = identifier
		}
		out = append(out, RegisteredQuotaProviderInfo{
			PluginID:           record.id,
			Provider:           identifier,
			DisplayName:        displayName,
			SupportedProviders: append([]string(nil), supported...),
			SupportsReset:      description.SupportsReset,
		})
	}
	return out
}

func normalizedQuotaProviders(providers []string) []string {
	seen := make(map[string]struct{}, len(providers))
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		provider = normalizeProviderID(provider)
		if provider == "" {
			continue
		}
		if _, duplicate := seen[provider]; duplicate {
			continue
		}
		seen[provider] = struct{}{}
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// HasQuotaProvider reports whether an active plugin handles a provider key.
func (h *Host) HasQuotaProvider(provider string) bool {
	return h.HasQuotaProviderContext(context.Background(), provider)
}

// HasQuotaProviderContext is HasQuotaProvider with cancellation support.
func (h *Host) HasQuotaProviderContext(ctx context.Context, provider string) bool {
	return h.quotaProviderRecord(ctx, provider) != nil
}

// HasQuotaProviderForPlugin reports whether a plugin exposes quota capability.
func (h *Host) HasQuotaProviderForPlugin(pluginID string) bool {
	return h.quotaProviderRecordByPlugin(pluginID) != nil
}

// QuotaSupportedProvidersSet returns all provider keys declared by active quota plugins.
func (h *Host) QuotaSupportedProvidersSet(ctx context.Context) map[string]struct{} {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(map[string]struct{})
	for _, record := range h.activeRecords() {
		if errContext := ctx.Err(); errContext != nil {
			break
		}
		provider := record.plugin.Capabilities.QuotaProvider
		if provider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
			continue
		}
		identifier, okIdentifier := h.callQuotaIdentifier(record, provider)
		if h.isPluginFused(record.id) {
			continue
		}
		if okIdentifier {
			out[identifier] = struct{}{}
		}
		if pluginID := normalizeProviderID(record.id); pluginID != "" {
			out[pluginID] = struct{}{}
		}
		if authProvider := record.plugin.Capabilities.AuthProvider; authProvider != nil {
			if authID, okAuth := h.callAuthProviderIdentifier(record.id, authProvider); okAuth && authID != "" {
				out[authID] = struct{}{}
			}
		}
		description, handled, errDescribe := h.callQuotaDescribe(ctx, record, provider)
		if errDescribe != nil || !handled {
			continue
		}
		for _, supported := range normalizedQuotaProviders(description.SupportedProviders) {
			out[supported] = struct{}{}
		}
	}
	return out
}

func (h *Host) quotaProviderRecord(ctx context.Context, provider string) *capabilityRecord {
	provider = normalizeProviderID(provider)
	if h == nil || provider == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	records := h.activeRecords()
	for _, record := range records {
		if ctx.Err() != nil {
			return nil
		}
		quotaProvider := record.plugin.Capabilities.QuotaProvider
		if quotaProvider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
			continue
		}
		identifier, okIdentifier := h.callQuotaIdentifier(record, quotaProvider)
		if h.isPluginFused(record.id) {
			continue
		}
		if (okIdentifier && identifier == provider) || normalizeProviderID(record.id) == provider {
			copyRecord := record
			return &copyRecord
		}
		if authProvider := record.plugin.Capabilities.AuthProvider; authProvider != nil {
			if authID, okAuth := h.callAuthProviderIdentifier(record.id, authProvider); okAuth && authID == provider {
				copyRecord := record
				return &copyRecord
			}
		}
	}
	for _, record := range records {
		if ctx.Err() != nil {
			return nil
		}
		quotaProvider := record.plugin.Capabilities.QuotaProvider
		if quotaProvider == nil || h.isPluginFused(record.id) {
			continue
		}
		description, handled, errDescribe := h.callQuotaDescribe(ctx, record, quotaProvider)
		if errDescribe != nil || !handled {
			continue
		}
		for _, supported := range description.SupportedProviders {
			if normalizeProviderID(supported) == provider {
				copyRecord := record
				return &copyRecord
			}
		}
	}
	return nil
}

func (h *Host) quotaProviderRecordByPlugin(pluginID string) *capabilityRecord {
	pluginID = strings.TrimSpace(pluginID)
	if h == nil || pluginID == "" {
		return nil
	}
	for _, record := range h.activeRecords() {
		if record.id == pluginID && record.plugin.Capabilities.QuotaProvider != nil && !h.isPluginFused(record.id) {
			copyRecord := record
			return &copyRecord
		}
	}
	return nil
}

// DescribeQuota returns one plugin's quota capability description.
func (h *Host) DescribeQuota(
	ctx context.Context,
	pluginID string,
) (pluginapi.QuotaDescribeResponse, bool, error) {
	record := h.quotaProviderRecordByPlugin(pluginID)
	if record == nil {
		record = h.quotaProviderRecord(ctx, pluginID)
	}
	if record == nil {
		return pluginapi.QuotaDescribeResponse{}, false, nil
	}
	return h.callQuotaDescribe(ctx, *record, record.plugin.Capabilities.QuotaProvider)
}

// FetchQuota asks the highest-priority matching plugin before callers consider declarative probes.
func (h *Host) FetchQuota(
	ctx context.Context,
	req pluginapi.QuotaFetchRequest,
) (pluginapi.QuotaFetchResponse, bool, error) {
	record := h.quotaProviderRecord(ctx, req.Provider)
	if record == nil {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	return h.callQuotaFetch(ctx, *record, record.plugin.Capabilities.QuotaProvider, req)
}

// FetchQuotaByPlugin targets a specific plugin quota provider.
func (h *Host) FetchQuotaByPlugin(
	ctx context.Context,
	pluginID string,
	req pluginapi.QuotaFetchRequest,
) (pluginapi.QuotaFetchResponse, bool, error) {
	record := h.quotaProviderRecordByPlugin(pluginID)
	if record == nil {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	return h.callQuotaFetch(ctx, *record, record.plugin.Capabilities.QuotaProvider, req)
}

// ResetQuota invokes a matching plugin only when the caller has separately authorized reset.
func (h *Host) ResetQuota(
	ctx context.Context,
	req pluginapi.QuotaResetRequest,
) (pluginapi.QuotaResetResponse, bool, error) {
	record := h.quotaProviderRecord(ctx, req.Provider)
	if record == nil {
		return pluginapi.QuotaResetResponse{}, false, nil
	}
	return h.callQuotaReset(ctx, *record, record.plugin.Capabilities.QuotaProvider, req)
}

// ResetQuotaByPlugin targets a specific plugin quota provider.
func (h *Host) ResetQuotaByPlugin(
	ctx context.Context,
	pluginID string,
	req pluginapi.QuotaResetRequest,
) (pluginapi.QuotaResetResponse, bool, error) {
	record := h.quotaProviderRecordByPlugin(pluginID)
	if record == nil {
		return pluginapi.QuotaResetResponse{}, false, nil
	}
	return h.callQuotaReset(ctx, *record, record.plugin.Capabilities.QuotaProvider, req)
}

func (h *Host) callQuotaIdentifier(
	record capabilityRecord,
	provider pluginapi.QuotaProvider,
) (identifier string, ok bool) {
	if h == nil || provider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return "", false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "QuotaProvider.Identifier", recovered)
			identifier = ""
			ok = false
		}
	}()
	identifier = normalizeProviderID(provider.Identifier())
	return identifier, identifier != ""
}

func (h *Host) callQuotaDescribe(
	ctx context.Context,
	record capabilityRecord,
	provider pluginapi.QuotaProvider,
) (resp pluginapi.QuotaDescribeResponse, handled bool, err error) {
	if h == nil || provider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.QuotaDescribeResponse{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "QuotaProvider.DescribeQuota", recovered)
			resp = pluginapi.QuotaDescribeResponse{}
			handled = false
			err = fmt.Errorf("plugin %s panicked during DescribeQuota: %v", record.id, recovered)
		}
	}()
	response, errDescribe := provider.DescribeQuota(ctx, pluginapi.QuotaDescribeRequest{
		Plugin: clonePluginMetadata(record.meta),
	})
	if quotaOperationUnsupported(errDescribe) {
		return pluginapi.QuotaDescribeResponse{}, false, nil
	}
	if errDescribe != nil {
		return pluginapi.QuotaDescribeResponse{}, true, errDescribe
	}
	response.SupportedProviders = normalizedQuotaProviders(response.SupportedProviders)
	return response, true, nil
}

func (h *Host) callQuotaFetch(
	ctx context.Context,
	record capabilityRecord,
	provider pluginapi.QuotaProvider,
	req pluginapi.QuotaFetchRequest,
) (resp pluginapi.QuotaFetchResponse, handled bool, err error) {
	if h == nil || provider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req.Provider = normalizeProviderID(req.Provider)
	req.StorageJSON = bytes.Clone(req.StorageJSON)
	req.Metadata = cloneInterceptorMetadata(req.Metadata)
	req.Attributes = cloneStringMap(req.Attributes)
	req.Host = h.hostConfigSummary()
	if req.HTTPClient == nil {
		req.HTTPClient = h.newHTTPClient(nil, req.Provider)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "QuotaProvider.FetchQuota", recovered)
			resp = pluginapi.QuotaFetchResponse{}
			handled = false
			err = fmt.Errorf("plugin %s panicked during FetchQuota: %v", record.id, recovered)
		}
	}()
	response, errFetch := provider.FetchQuota(ctx, req)
	if quotaOperationUnsupported(errFetch) {
		return pluginapi.QuotaFetchResponse{}, false, nil
	}
	if errFetch != nil {
		return pluginapi.QuotaFetchResponse{}, true, errFetch
	}
	return cloneQuotaFetchResponse(response), true, nil
}

func (h *Host) callQuotaReset(
	ctx context.Context,
	record capabilityRecord,
	provider pluginapi.QuotaProvider,
	req pluginapi.QuotaResetRequest,
) (resp pluginapi.QuotaResetResponse, handled bool, err error) {
	if h == nil || provider == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return pluginapi.QuotaResetResponse{}, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req.Provider = normalizeProviderID(req.Provider)
	req.StorageJSON = bytes.Clone(req.StorageJSON)
	req.Metadata = cloneInterceptorMetadata(req.Metadata)
	req.Attributes = cloneStringMap(req.Attributes)
	req.Host = h.hostConfigSummary()
	if req.HTTPClient == nil {
		req.HTTPClient = h.newHTTPClient(nil, req.Provider)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "QuotaProvider.ResetQuota", recovered)
			resp = pluginapi.QuotaResetResponse{}
			handled = false
			err = fmt.Errorf("plugin %s panicked during ResetQuota: %v", record.id, recovered)
		}
	}()
	response, errReset := provider.ResetQuota(ctx, req)
	if quotaOperationUnsupported(errReset) {
		return pluginapi.QuotaResetResponse{}, false, nil
	}
	if errReset != nil {
		return pluginapi.QuotaResetResponse{}, true, errReset
	}
	return response, true, nil
}

func quotaOperationUnsupported(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, pluginapi.ErrQuotaUnsupported) || errors.Is(err, errors.ErrUnsupported) {
		return true
	}
	var rpcErr rpcError
	if errors.As(err, &rpcErr) {
		switch strings.ToLower(strings.TrimSpace(rpcErr.Code)) {
		case "unsupported", "not_supported", "unsupported_method", "method_not_found", "unknown_method":
			return true
		}
	}
	return false
}

func cloneQuotaFetchResponse(src pluginapi.QuotaFetchResponse) pluginapi.QuotaFetchResponse {
	out := src
	if src.Subscription != nil {
		subscription := *src.Subscription
		out.Subscription = &subscription
	}
	if len(src.Groups) == 0 {
		out.Groups = nil
		return out
	}
	out.Groups = make([]pluginapi.QuotaGroup, len(src.Groups))
	for index, group := range src.Groups {
		out.Groups[index] = group
		out.Groups[index].Buckets = append([]pluginapi.QuotaBucket(nil), group.Buckets...)
	}
	return out
}
