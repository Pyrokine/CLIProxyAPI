package proxyutil

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestURLValidationErrorsHideCredentialsAndPreserveCause(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	for _, raw := range []string{
		"http://fixture-user:fixture-password@proxy.invalid:bad",
		"http://fixture-user:fixture-password@proxy%zz.invalid:8080",
		"http://fixture-user:fixture-password@proxy.invalid:bad?api_key=fixture-key",
	} {
		for _, validate := range []struct {
			name string
			run  func(string) error
		}{
			{name: "proxy", run: ValidateProxyURL},
			{name: "HTTP", run: ValidateHTTPURL},
			{name: "public proxy compatibility", run: ValidatePublicProxyURL},
			{name: "public HTTP", run: ValidatePublicHTTPURL},
		} {
			t.Run(
				validate.name+"/"+strings.Split(raw, "@")[1], func(t *testing.T) {
					err := validate.run(raw)
					if err == nil {
						t.Fatal("malformed URL accepted")
					}
					for _, rendered := range []string{err.Error(), fmt.Sprint(err), fmt.Sprintf("%+v", err)} {
						for _, secret := range []string{"fixture-user", "fixture-password", "fixture-key", raw} {
							if strings.Contains(rendered, secret) {
								t.Fatal("validation error exposed URL or credentials")
							}
						}
					}
					var parseError *url.Error
					if !errors.As(err, &parseError) || parseError.URL != raw || !errors.Is(err, parseError.Err) {
						t.Fatal("original typed parse cause was lost")
					}
				},
			)
		}
	}
}
