package platformclient

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/ang-ee/angee-operator/api"
)

func (p *RemoteClient) BackupPreview(ctx context.Context, req api.BackupRequest) (api.BackupPreview, error) {
	var preview api.BackupPreview
	err := p.doJSON(ctx, http.MethodPost, "/backups/preview", nil, req, &preview)
	return preview, err
}
func (p *RemoteClient) BackupStart(ctx context.Context, req api.BackupRequest) (api.JobRunOperation, error) {
	var receipt api.JobRunOperation
	err := p.doJSON(ctx, http.MethodPost, "/backups", nil, req, &receipt)
	return receipt, err
}
func (p *RemoteClient) BackupArchive(ctx context.Context, id string) (io.ReadCloser, api.BackupArtifact, error) {
	op, err := p.JobRunGet(ctx, id)
	if err != nil {
		return nil, api.BackupArtifact{}, err
	}
	if op.Artifact == nil {
		return nil, api.BackupArtifact{}, &RemoteError{Status: http.StatusConflict}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint("/backups/"+url.PathEscape(id)+"/archive", nil), nil)
	if err != nil {
		return nil, api.BackupArtifact{}, err
	}
	response, err := p.streamingHTTPClient().Do(request)
	if err != nil {
		return nil, api.BackupArtifact{}, err
	}
	if response.StatusCode >= 300 {
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, api.BackupArtifact{}, err
		}
		return nil, api.BackupArtifact{}, operatorHTTPError(response.StatusCode, body)
	}
	return response.Body, *op.Artifact, nil
}
