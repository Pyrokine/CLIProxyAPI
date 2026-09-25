package registry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const maxDevinModelsSize = 8 << 20

var devinModelsURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/devin_models.json",
	"https://models.router-for.me/devin_models.json",
}

var devinModelsUpdaterOnce sync.Once

// StartDevinModelsUpdater starts the bounded periodic Devin catalog refresh.
func StartDevinModelsUpdater(ctx context.Context, refreshHours int) {
	interval, enabled := modelRefreshInterval(refreshHours)
	if !enabled {
		return
	}
	devinModelsUpdaterOnce.Do(func() {
		go runDevinModelsUpdater(ctx, interval)
	})
}

func runDevinModelsUpdater(ctx context.Context, interval time.Duration) {
	tryRefreshDevinModels(ctx, "startup Devin model refresh")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Infof("periodic Devin model refresh started (interval=%s)", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tryRefreshDevinModels(ctx, "periodic Devin model refresh")
		}
	}
}

func tryRefreshDevinModels(ctx context.Context, label string) {
	data, sourceURL, err := fetchDevinModelsFromRemote(ctx)
	if err != nil {
		log.Warnf("%s: %v, keeping current data", label, err)
		return
	}
	changed, err := loadDevinModelsFromBytes(data, sourceURL)
	if err != nil {
		log.Warnf("%s: fetched catalog rejected, keeping current data: %v", label, err)
		return
	}
	if !changed {
		log.Infof("%s completed from %s, no changes detected", label, sourceURL)
		return
	}
	log.Infof("%s completed from %s, catalog updated", label, sourceURL)
	notifyModelRefresh([]string{"devin"})
}

func fetchDevinModelsFromRemote(ctx context.Context) ([]byte, string, error) {
	client := &http.Client{Timeout: modelsFetchTimeout}
	var lastErr error
	for _, sourceURL := range devinModelsURLs {
		requestContext, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
		request, err := http.NewRequestWithContext(requestContext, http.MethodGet, sourceURL, nil)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("create request for %s: %w", sourceURL, err)
			continue
		}

		response, err := client.Do(request)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("fetch %s: %w", sourceURL, err)
			continue
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			cancel()
			lastErr = fmt.Errorf("fetch %s: unexpected status %d", sourceURL, response.StatusCode)
			continue
		}
		if response.ContentLength > maxDevinModelsSize {
			_ = response.Body.Close()
			cancel()
			lastErr = fmt.Errorf("fetch %s: payload exceeds %d bytes", sourceURL, maxDevinModelsSize)
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxDevinModelsSize+1))
		closeErr := response.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = fmt.Errorf("read %s: %w", sourceURL, readErr)
			continue
		}
		if closeErr != nil {
			lastErr = fmt.Errorf("close %s response: %w", sourceURL, closeErr)
			continue
		}
		if len(body) > maxDevinModelsSize {
			lastErr = fmt.Errorf("fetch %s: payload exceeds %d bytes", sourceURL, maxDevinModelsSize)
			continue
		}
		return body, sourceURL, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no Devin model catalog URLs configured")
	}
	return nil, "", lastErr
}
