package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/store"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

const updateTestDataFormats = "management=1:5:5;usage=1:9:9;postgres=1:1:1"

func updateSessionWithTestDataFormats(
	t *testing.T,
	session updateGuardSession,
	previous, target string,
) updateGuardSession {
	t.Helper()
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	values["data_formats"] = map[string]int{"management": 5, "usage": 9, "postgres": 1}
	values["previous_identity"].(map[string]any)["data_formats"] = previous
	values["target_identity"].(map[string]any)["data_formats"] = target
	raw, err = json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestUpdateGuardChecksTargetAndRollbackDataFormatsBeforeInstallation(t *testing.T) {
	for _, test := range []struct {
		name, previous, target string
		valid                  bool
	}{
		{"same formats", updateTestDataFormats, updateTestDataFormats, true},
		{"target cannot read usage", updateTestDataFormats, "management=1:5:5;usage=1:8:8;postgres=1:1:1", false},
		{
			"rollback cannot read migrated usage", updateTestDataFormats,
			"management=1:5:5;usage=1:10:10;postgres=1:1:1", false,
		},
		{"management migration", updateTestDataFormats, "management=1:6:6;usage=1:9:9;postgres=1:1:1", false},
		{"postgres format change", updateTestDataFormats, "management=1:5:5;usage=1:9:9;postgres=1:2:2", false},
		{"target declaration missing", updateTestDataFormats, "", false},
		{"rollback declaration missing", "", updateTestDataFormats, false},
		{"duplicate component", updateTestDataFormats, updateTestDataFormats + ";usage=1:9:9", false},
		{"invalid numeric format", updateTestDataFormats, "management=1:5:5;usage=one:9:9;postgres=1:1:1", false},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				guardStore := openUpdateGuardTestStore(t)
				now := time.Now().UTC()
				session, _ := prepareArmedUpdateGuardSession(
					t,
					guardStore,
					"http://127.0.0.1:8317",
					now,
					"v1.0.0",
					"v1.0.1",
				)
				session = updateSessionWithTestDataFormats(t, session, test.previous, test.target)
				err := validateUpdateGuardSession(session, guardStore.path, session.ExecutablePath, true, now)
				if (err == nil) != test.valid {
					t.Fatalf("data compatibility error=%v valid=%v", err, test.valid)
				}
				current, err := os.ReadFile(session.ExecutablePath)
				if err != nil || string(current) != "old-binary" {
					t.Fatalf("validation replaced the current executable: %q, %v", current, err)
				}
			},
		)
	}
}

func TestUpdatePrivateHealthRequiresReadableManagementData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, fault := range []string{"none", "future schema", "missing quota column"} {
		t.Run(
			fault, func(t *testing.T) {
				guardStore := openUpdateGuardTestStore(t)
				session, token := prepareArmedUpdateGuardSession(
					t, guardStore, "http://127.0.0.1:8317", time.Now(), "v1.0.0", "v1.0.1",
				)
				session = updateSessionWithTestDataFormats(
					t,
					session,
					updateTestDataFormats,
					updateTestDataFormats,
				)
				session.DataFormats = map[string]int{"management": 5}
				raw, err := json.Marshal(session)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := guardStore.db.ExecContext(
					t.Context(), "UPDATE state_items SET value=? WHERE key=?", raw, updateSessionStateKey,
				); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "future schema":
					_, err = guardStore.db.ExecContext(
						t.Context(),
						"UPDATE meta SET value='6' WHERE key='schema_version'",
					)
				case "missing quota column":
					_, err = guardStore.db.ExecContext(
						t.Context(),
						"ALTER TABLE quota_entries_v2 RENAME COLUMN auth_index TO unreadable_auth_index",
					)
				}
				if err != nil {
					t.Fatal(err)
				}
				handler := &Handler{stateStore: guardStore}
				router := gin.New()
				router.GET(updateHealthPath, handler.GetUpdateHealth)
				request := httptest.NewRequest(http.MethodGet, updateHealthPath, nil)
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set(updateHealthTokenHeader, token)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				want := http.StatusServiceUnavailable
				if fault == "none" {
					want = http.StatusOK
				}
				if response.Code != want {
					t.Fatalf(
						"health accepted unreadable data: status=%d want=%d body=%s",
						response.Code,
						want,
						response.Body,
					)
				}
				if fault == "none" && !strings.Contains(response.Body.String(), `"data_ready":true`) {
					t.Fatalf("health omitted confirmed data readiness: %s", response.Body)
				}
			},
		)
	}
}

func TestUpdateDrainRechecksDataBeforeInstallation(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(
			strconv.FormatBool(changed), func(t *testing.T) {
				state := openUpdateGuardTestStore(t)
				admission := executionregistry.NewAdmission()
				session, token := prepareArmedUpdateGuardSession(
					t, state, "http://127.0.0.1:8317", time.Now(), "v1.0.0", "v1.0.1",
				)
				confirmation, err := admission.Drain(t.Context(), session.AttemptID, session.Deadline)
				if err != nil {
					t.Fatal(err)
				}
				_, err = state.transitionUpdateSession(
					t.Context(), session.AttemptID, updatePhaseLaunching, func(next *updateGuardSession) {
						next.Phase = updatePhaseInstalling
						next.OriginInstance = admission.InstanceID()
						next.DrainConfirmation = &confirmation
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if changed {
					if _, err := state.db.ExecContext(
						t.Context(), "UPDATE meta SET value='6' WHERE key='schema_version'",
					); err != nil {
						t.Fatal(err)
					}
				}
				handler := &Handler{stateStore: state, inferenceAdmission: admission}
				router := gin.New()
				router.POST(updateDrainPath, handler.PostUpdateDrain)
				body := `{"attempt_id":"` + session.AttemptID + `","action":"confirm"}`
				request := httptest.NewRequest(http.MethodPost, updateDrainPath, strings.NewReader(body))
				request.RemoteAddr = "127.0.0.1:12345"
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(updateHealthTokenHeader, token)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				want := http.StatusOK
				if changed {
					want = http.StatusServiceUnavailable
				}
				if response.Code != want {
					t.Fatalf(
						"changed=%v confirm status=%d want=%d body=%s",
						changed,
						response.Code,
						want,
						response.Body,
					)
				}
			},
		)
	}
}

func TestUpdateDataFailurePreservesActionableStatus(t *testing.T) {
	session := updateGuardSession{
		Phase: updatePhaseError, ResultCode: normalizeUpdateResultCode("data_compatibility_failed"), Percent: 85,
		AttemptID: strings.Repeat("a", 32),
	}
	state := updateStateFromSession(session)
	if state.status != "error" || !strings.Contains(
		state.message, "Database compatibility",
	) || state.attemptID != session.AttemptID || state.percent != 85 {
		t.Fatalf(
			"database compatibility failure: status=%q message=%q attempt=%q percent=%d",
			state.status,
			state.message,
			state.attemptID,
			state.percent,
		)
	}
}

func TestUpdateDataDeclarationMatchesCurrentStores(t *testing.T) {
	formats, err := parseUpdateDataFormats(buildinfo.DataFormats)
	if err != nil {
		t.Fatal(err)
	}
	managementVersion, err := strconv.Atoi(managementStateSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	usageVersion, err := strconv.Atoi(usage.CurrentSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".goreleaser.yml", "Dockerfile"} {
		content, err := os.ReadFile(filepath.Join("..", "..", "..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		var flags []string
		switch name {
		case ".goreleaser.yml":
			var release struct {
				Builds []struct {
					LDFlags []string `yaml:"ldflags"`
				} `yaml:"builds"`
			}
			if err := yaml.Unmarshal(content, &release); err != nil {
				t.Fatal(err)
			}
			for _, build := range release.Builds {
				flags = append(flags, strings.Join(build.LDFlags, " "))
			}
		case "Dockerfile":
			canonical := strings.ReplaceAll(string(content), "\r\n", "\n")
			for _, lineEnding := range []string{"\n", "\r\n"} {
				source := strings.ReplaceAll(canonical, "\n", lineEnding)
				logical := strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\\\n", "")
				_, value, found := strings.Cut(logical, `-ldflags="`)
				if !found {
					t.Fatal("Dockerfile does not set linker flags")
				}
				value, _, found = strings.Cut(value, `"`)
				if !found {
					t.Fatal("Dockerfile linker flags are not terminated")
				}
				flags = append(flags, value)
			}
		}
		if len(flags) == 0 {
			t.Fatalf("%s does not declare any build linker flags", name)
		}
		for index, value := range flags {
			var declared string
			for _, match := range updateLinkerIdentityPattern.FindAllStringSubmatch(value, -1) {
				if match[1] == "github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo.DataFormats" {
					declared = match[2]
				}
			}
			if declared != buildinfo.DataFormats {
				t.Fatalf("%s build %d database formats = %q, want %q", name, index, declared, buildinfo.DataFormats)
			}
		}
	}
	for name, current := range map[string]int{
		"management": managementVersion, "usage": usageVersion, "postgres": store.PostgresDataFormatVersion,
	} {
		if got := formats[name]; got != [3]int{1, current, current} {
			t.Fatalf("%s build declaration=%v current=%d", name, got, current)
		}
	}
	session := updateSessionWithTestDataFormats(
		t, updateGuardSession{
			PreviousIdentity: *updateGuardTestIdentity("old", "v1.0.0"),
			TargetIdentity:   *updateGuardTestIdentity("new", "v1.0.1"),
		}, updateTestDataFormats, "management=1:5:5;usage=1:10:10;postgres=1:2:2",
	)
	session.DataFormats = map[string]int{"management": managementVersion}
	if err := validateUpdateDataFormats(session); err != nil {
		t.Fatalf("an unused database blocked a compatible update: %v", err)
	}
}

func TestUpdateDataReadinessUsesActualUsageQuery(t *testing.T) {
	for _, fault := range []string{"none", "future schema", "missing event column", "unavailable persister"} {
		t.Run(
			fault, func(t *testing.T) {
				dir := t.TempDir()
				persister := usage.NewPersister(dir, config.UsageRetention{Days: -1})
				persister.Start(t.Context())
				t.Cleanup(persister.Stop)
				if !persister.IsReady() {
					t.Fatal("temporary usage store did not initialize")
				}
				db, err := sql.Open("sqlite", filepath.Join(dir, "events.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				switch fault {
				case "future schema":
					_, err = db.ExecContext(
						t.Context(),
						"UPDATE meta SET value='10' WHERE key='schema_version'",
					)
				case "missing event column":
					_, err = db.ExecContext(
						t.Context(), "ALTER TABLE events RENAME COLUMN input_tokens TO unreadable_input_tokens",
					)
				}
				if err != nil {
					t.Fatal(err)
				}
				handler := &Handler{
					stateStore: openUpdateGuardTestStore(t), usagePersister: persister,
					cfg: &config.Config{UsageDataDir: dir},
				}
				if fault == "unavailable persister" {
					handler.usagePersister = nil
				}
				formats, err := handler.updateDataReadiness(t.Context())
				if (err == nil) != (fault == "none") {
					t.Fatalf("fault=%s readiness error=%v formats=%v", fault, err, formats)
				}
				if fault == "none" && formats["usage"] != 9 {
					t.Fatalf("usage format missing: %v", formats)
				}
			},
		)
	}
}

func TestUpdateDataReadinessChecksSelectedPostgresStore(t *testing.T) {
	handler := &Handler{stateStore: openUpdateGuardTestStore(t), tokenStore: &store.PostgresStore{}}
	if _, err := handler.updateDataReadiness(t.Context()); err == nil {
		t.Fatal("selected PostgreSQL store was not checked for readiness")
	}
}

func TestUpdatePrivateHealthProbeRejectsUnconfirmedDataReadiness(t *testing.T) {
	identity := *updateGuardTestIdentity("new-binary", "v1.0.1")
	session := updateGuardSession{
		SchemaVersion:  updateGuardStateSchemaVersion,
		AttemptID:      strings.Repeat("a", 32),
		TargetVersion:  identity.Tag,
		TargetIdentity: identity,
		HealthBaseURL:  "http://127.0.0.1:8317",
	}
	session = updateSessionWithTestDataFormats(t, session, updateTestDataFormats, updateTestDataFormats)
	identity = session.TargetIdentity
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"status":"ok","version":"v1.0.1","attempt_id":"` + session.AttemptID + `","identity":` + string(
		raw,
	) + `}`
	client := &http.Client{
		Transport: updateGuardRoundTripFunc(
			func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(payload)), Request: request,
				}, nil
			},
		),
	}
	if err := probeUpdatePrivateHealth(
		context.Background(), client, session, "synthetic-token", session.TargetVersion,
	); err == nil {
		t.Fatal("health probe accepted a process that did not confirm business data readiness")
	}
}
