package config

import (
	_ "embed"
	"errors"
)

// exampleConfig is the documented starting point shipped with the binary. Keeping
// it inside the package means "print-example" and the repository file can never
// drift apart, and a test validates it on every run.
//
//go:embed example/config.example.json
var exampleConfig []byte

// Example returns the example configuration bytes.
func Example() ([]byte, error) {
	if len(exampleConfig) == 0 {
		return nil, errors.New("the example configuration is not available")
	}
	return exampleConfig, nil
}

// ExampleValidated parses the example configuration, which is how the tests prove
// the shipped example is a usable configuration rather than decoration.
func ExampleValidated() (*Config, error) {
	example, err := Example()
	if err != nil {
		return nil, err
	}
	return Parse(example)
}
