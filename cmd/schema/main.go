package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/invopop/jsonschema"
)

func main() {
	output := flag.String("o", "", "write schema to path instead of stdout")
	flag.Parse()

	reflector := jsonschema.Reflector{
		BaseSchemaID: jsonschema.ID("https://docs.angee.ai/angee.schema.json"),
	}
	schema := reflector.Reflect(&manifest.Stack{})
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "marshal schema: %v\n", err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if *output == "" {
		_, _ = os.Stdout.Write(data)
		return
	}
	if err := os.WriteFile(*output, data, 0o644); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "write schema: %v\n", err)
		os.Exit(1)
	}
}
