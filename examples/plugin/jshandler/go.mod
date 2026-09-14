module github.com/router-for-me/CLIProxyAPIPlugins/jshandler

go 1.27.0

require (
	github.com/Pyrokine/CLIProxyAPI/v6 v6.0.0
	github.com/dop251/goja v0.0.0-20260906210903-70ad66ec7ce4
	github.com/sirupsen/logrus v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/dlclark/regexp2/v2 v2.8.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/Pyrokine/CLIProxyAPI/v6 => ../../..
