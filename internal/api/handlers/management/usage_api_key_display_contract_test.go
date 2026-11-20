package management

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

func TestUsageEventsKeepAliasAndTwoCharacterDisplayAcrossViewsAndFilters(t *testing.T) {
	const raw, aliased = "ab-synthetic-source-one-xy", "zz-synthetic-source-two-uv"
	persister := usage.NewPersister(t.TempDir(), config.UsageRetention{Days: -1})
	persister.Start(context.Background())
	defer persister.Stop()
	if !persister.IsReady() {
		t.Fatal("temporary SQLite unavailable")
	}
	h := &Handler{
		usagePersister: persister, cfg: &config.Config{
			APIKeys: []string{raw, aliased}, APIKeyAliases: map[string]string{aliased: "Source_A-9"},
		},
	}
	body := (`[{"timestamp":"2026-11-06T07:01:00Z","model":"synthetic","api_key":` +
		`"`) +
		raw +
		(`","tokens":` +
			`{"total_tokens":` +
			`1}},{"timestamp":` +
			`"2026-11-06T07:` +
			`02:00Z","model":` +
			`"synthetic",` +
			`"api_key":` +
			`"`) +
		aliased +
		(`","tokens":` +
			`{"total_tokens` +
			`":2}}]`)
	c, response := newArtifactCacheTestContext(http.MethodPost, "/usage/import", body)
	h.ImportUsageStatistics(c)
	if response.Code != 200 {
		t.Fatalf("import status=%d body=%s", response.Code, response.Body.String())
	}
	for _, view := range []string{"", "&view=compact"} {
		for _, filter := range []string{"", "&api_key=ab...xy", "&api_key=" + util.HideAPIKey(raw)} {
			c, response := newArtifactCacheTestContext(
				http.MethodGet,
				"/usage/events?from=2026-11-06T07:00:00Z&to=2026-11-06T08:00:00Z&page_size=20"+view+filter, "",
			)
			h.GetUsageEvents(c)
			if response.Code != 200 {
				t.Fatalf(
					"view=%q filter=%q status=%d body=%s",
					view,
					filter,
					response.Code,
					response.Body.String(),
				)
			}
			var result struct {
				Total  int `json:"total"`
				Events []struct {
					Key   string `json:"api_key"`
					Alias string `json:"api_key_alias"`
				} `json:"events"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := 2
			if filter != "" {
				want = 1
			}
			if result.Total != want || len(result.Events) != want {
				t.Fatalf("view=%q filter=%q result=%+v", view, filter, result)
			}
			for _, event := range result.Events {
				if event.Key != "ab...xy" && event.Key != "zz...uv" ||
					event.Key == "zz...uv" && event.Alias != "Source_A-9" {
					t.Fatalf("display/alias changed: %+v", event)
				}
			}
			if strings.Contains(response.Body.String(), raw) ||
				strings.Contains(response.Body.String(), aliased) {
				t.Fatal("usage response disclosed original client key")
			}
		}
	}
}
