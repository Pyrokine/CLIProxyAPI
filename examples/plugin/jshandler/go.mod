module github.com/router-for-me/CLIProxyAPIPlugins/jshandler

go 1.27.4

require (
	github.com/Pyrokine/CLIProxyAPI/v6 v6.10.9-aug.4
	github.com/dop251/goja v0.0.0-20261124180248-00ef7861e082
	github.com/sirupsen/logrus v1.10.2
	go.yaml.in/yaml/v4 v4.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/dlclark/regexp2/v2 v2.13.2 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20261118010117-084b33a85f63 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
)

replace github.com/Pyrokine/CLIProxyAPI/v6 => ../../..
