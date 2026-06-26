module github.com/credo-go/credo/examples/hello

go 1.27

toolchain go1.27rc1

require github.com/credo-go/credo v0.0.0

require (
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/credo-go/credo => ../..
