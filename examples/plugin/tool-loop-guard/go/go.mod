module github.com/router-for-me/CLIProxyAPI/v7/examples/plugin/tool-loop-guard/go

go 1.26.10

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/MoeForge/moego v0.1.0-alpha.5 // indirect

replace github.com/router-for-me/CLIProxyAPI/v7 => ../../../..
