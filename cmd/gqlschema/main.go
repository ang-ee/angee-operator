// Command gqlschema writes the operator's GraphQL schema as SDL, for frontend
// codegen (e.g. graphql-codegen / @refinedev/nestjs-query). It formats the same
// executable schema the operator serves, so the emitted SDL can never drift
// from the running API. The operator also exposes live introspection at
// /graphql; this file is the committed, offline-consumable equivalent.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ang-ee/angee-operator/internal/operator/gql"
	"github.com/vektah/gqlparser/v2/formatter"
)

func main() {
	output := flag.String("o", "", "write SDL to path instead of stdout")
	flag.Parse()

	schema := gql.NewExecutableSchema(gql.Config{Resolvers: &gql.Resolver{}}).Schema()

	w := os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", *output, err)
			os.Exit(1)
		}
		defer f.Close()
		w = f
	}
	formatter.NewFormatter(w).FormatSchema(schema)
}
