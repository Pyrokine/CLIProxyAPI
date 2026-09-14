package api

import (
	"net/http"
	"testing"
)

func TestQuotaRefreshTaskRoutesRegistered(t *testing.T) {
	server := newTestServerWithOptions(t, WithLocalManagementPassword("test-management-password"))
	want := map[string]bool{
		http.MethodPost + " /v0/management/quota/refresh":                       false,
		http.MethodGet + " /v0/management/quota/refresh-tasks":                  false,
		http.MethodGet + " /v0/management/quota/refresh-tasks/:task_id":         false,
		http.MethodPost + " /v0/management/quota/refresh-tasks/:task_id/pause":  false,
		http.MethodPost + " /v0/management/quota/refresh-tasks/:task_id/resume": false,
		http.MethodPost + " /v0/management/quota/refresh-tasks/:task_id/cancel": false,
	}
	for _, route := range server.engine.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := want[key]; exists {
			want[key] = true
		}
	}
	for route, registered := range want {
		if !registered {
			t.Errorf("quota refresh task route %s is not registered", route)
		}
	}
}
