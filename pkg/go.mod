module github.com/zodixdev/infrapilot/pkg

go 1.27.2

replace github.com/zodixdev/infrapilot/gen => ../gen

require (
	github.com/google/uuid v1.6.0
	github.com/stretchr/testify v1.12.1
	github.com/zodixdev/infrapilot/gen v0.0.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
