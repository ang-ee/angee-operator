package operator

import (
	"testing"

	opgql "github.com/ang-ee/angee-operator/internal/operator/gql"
	"github.com/vektah/gqlparser/v2"
)

// TestHasuraProviderDocumentsValidate is the consumability guarantee for
// @refinedev/hasura: it validates the exact GraphQL documents that provider
// generates (namingConvention "hasura-default", idType "String") against the
// operator's executable schema. If any document fails to validate, a refine
// console wired to this operator would break — so this test must stay green.
//
// Documents are reconstructed from the provider's query builders
// (packages/hasura/src/dataProvider, utils/*). Field selections are kept minimal
// (id, name) — what matters is that root fields, argument names/types, variable
// types (services_bool_exp, [services_order_by!], services_insert_input,
// services_pk_columns_input, services_set_input, $id: String!) and the
// aggregate shape all exist and type-check.
func TestHasuraProviderDocumentsValidate(t *testing.T) {
	schema := opgql.NewExecutableSchema(opgql.Config{Resolvers: &opgql.Resolver{}}).Schema()

	docs := map[string]string{
		"getList": `query GetList($limit: Int, $offset: Int, $order_by: [services_order_by!], $where: services_bool_exp) {
			services(limit: $limit, offset: $offset, order_by: $order_by, where: $where) { id name }
			services_aggregate(where: $where) { aggregate { count } }
		}`,
		"getOne": `query GetOne($id: String!) {
			services_by_pk(id: $id) { id name }
		}`,
		"getMany": `query GetMany($where: services_bool_exp) {
			services(where: $where) { id name }
		}`,
		"create": `mutation Create($object: services_insert_input!) {
			insert_services_one(object: $object) { id name }
		}`,
		"update": `mutation Update($pk_columns: services_pk_columns_input!, $_set: services_set_input!) {
			update_services_by_pk(pk_columns: $pk_columns, _set: $_set) { id name }
		}`,
		"deleteOne": `mutation DeleteOne($id: String!) {
			delete_services_by_pk(id: $id) { id name }
		}`,
		"subscribeList": `subscription Sub($limit: Int, $offset: Int, $order_by: [services_order_by!], $where: services_bool_exp) {
			services(limit: $limit, offset: $offset, order_by: $order_by, where: $where) { id name }
		}`,
		// secrets exercise the other insert/set/pk_columns input types + filter ops.
		"createSecret": `mutation CreateSecret($object: secrets_insert_input!) {
			insert_secrets_one(object: $object) { id name }
		}`,
		"updateSecret": `mutation UpdateSecret($pk_columns: secrets_pk_columns_input!, $_set: secrets_set_input!) {
			update_secrets_by_pk(pk_columns: $pk_columns, _set: $_set) { id name }
		}`,
		"deleteSecret": `mutation DeleteSecret($id: String!) {
			delete_secrets_by_pk(id: $id) { id name }
		}`,
		"filterOps": `query Filter($where: sources_bool_exp) {
			sources(where: $where) { id name }
		}`,
		// NDC-shaped grouped aggregation (useGroupBy / useFacets via useCustom).
		"groupBy": `query GroupBy($where: services_bool_exp, $dimensions: [services_group_dimension!]!, $limit: Int, $offset: Int) {
			services_groups(where: $where, dimensions: $dimensions, limit: $limit, offset: $offset) {
				dimensions { key value }
				aggregates { count }
			}
		}`,
		"groupBySourcesNumeric": `query GroupBySources($dimensions: [sources_group_dimension!]!) {
			sources_groups(dimensions: $dimensions) {
				dimensions { key value }
				aggregates { count sum { ahead } avg { ahead } min { ahead } max { ahead } }
			}
		}`,
	}

	for name, doc := range docs {
		if _, errs := gqlparser.LoadQuery(schema, doc); len(errs) > 0 {
			t.Errorf("%s: refine-hasura document does not validate against schema: %v", name, errs)
		}
	}
}

// TestHasuraFilterOperatorsValidate checks the comparison operators the provider
// emits all exist on the comparison_exp inputs.
func TestHasuraFilterOperatorsValidate(t *testing.T) {
	schema := opgql.NewExecutableSchema(opgql.Config{Resolvers: &opgql.Resolver{}}).Schema()
	doc := `query Ops {
		services(where: {
			_and: [
				{ name: { _eq: "a", _neq: "b", _in: ["a"], _nin: ["b"], _like: "a%", _ilike: "%a%", _is_null: false } },
				{ _or: [{ status: { _gt: "x" } }, { status: { _lte: "y" } }] }
			]
		}) { id }
		sources(where: { ahead: { _gt: 0, _gte: 1, _lt: 9, _lte: 8 }, dirty: { _eq: true } }) { id }
	}`
	if _, errs := gqlparser.LoadQuery(schema, doc); len(errs) > 0 {
		t.Fatalf("filter-operator document does not validate: %v", errs)
	}
}
