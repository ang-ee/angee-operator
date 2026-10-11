package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"

	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/invopop/jsonschema"
)

func main() {
	output := flag.String("o", "", "write schema to path instead of stdout")
	flag.Parse()

	reflector := jsonschema.Reflector{
		BaseSchemaID: jsonschema.ID("https://docs.angee.ai/angee.schema.json"),
		Mapper:       mapType,
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

// mapType describes types whose YAML form the reflector cannot see: a source
// remote is a bare URL string or a mapping (manifest.SourceRemote.UnmarshalYAML).
func mapType(t reflect.Type) *jsonschema.Schema {
	if t != reflect.TypeFor[manifest.SourceRemote]() {
		return nil
	}
	properties := jsonschema.NewProperties()
	properties.Set("url", &jsonschema.Schema{Type: "string"})
	properties.Set("canonical", &jsonschema.Schema{Type: "boolean"})
	properties.Set("branches", &jsonschema.Schema{Type: "array", Items: &jsonschema.Schema{Type: "string"}})
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string"},
		{Type: "object", Properties: properties, Required: []string{"url"}, AdditionalProperties: jsonschema.FalseSchema},
	}}
}
