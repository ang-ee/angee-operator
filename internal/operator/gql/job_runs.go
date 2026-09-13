package gql

import (
	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/operator/gql/model"
	"strings"
)

func jobRunOperationModel(op api.JobRunOperation) *model.JobRunOperation {
	nodes := make([]*model.JobRunNode, 0, len(op.Nodes))
	for _, node := range op.Nodes {
		nodes = append(nodes, &model.JobRunNode{Name: node.Name, Kind: model.JobRunNodeKind(strings.ToUpper(node.Kind)), Status: model.JobRunStatus(strings.ToUpper(string(node.Status))), Message: optionalString(node.Message)})
	}
	return &model.JobRunOperation{ID: op.ID, RootJob: op.RootJob, ChainedRestart: op.ChainedRestart, Status: model.JobRunStatus(strings.ToUpper(string(op.Status))), CurrentStep: optionalString(op.CurrentStep), StartedAt: op.StartedAt, EndedAt: op.EndedAt, Nodes: nodes, Output: optionalString(op.Output), Error: optionalString(op.Error)}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
