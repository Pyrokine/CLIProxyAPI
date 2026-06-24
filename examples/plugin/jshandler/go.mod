module github.com/router-for-me/CLIProxyAPIPlugins/jshandler

go 1.26.0

require (
	github.com/Pyrokine/CLIProxyAPI/v6 v6.0.0
	github.com/dop251/goja v0.0.0-20260607120635-348e6bea910d
	github.com/sirupsen/logrus v1.9.4
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/dlclark/regexp2/v2 v2.2.1 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/Pyrokine/CLIProxyAPI/v6 => ../../..
