package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/dbtypes"
	"github.com/tgdrive/teldrive/v2/internal/importer"
	"github.com/tgdrive/teldrive/v2/internal/jobs"
)

func (h *Handler) ScanChannelImport(ctx context.Context, req *gen.ImportScanRequest) (gen.ScanChannelImportRes, error) {
	if h == nil || h.Importer == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if req == nil || req.ChannelId == 0 {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_import", "channelId is required", nil)
	}
	beforeID := req.BeforeId.Or(0)
	result, err := h.Importer.Scan(ctx, userID, req.ChannelId, beforeID, int(req.Limit.Or(100)))
	if err != nil {
		return nil, mapImportError(err)
	}
	response := gen.ImportScanResponse{Items: make([]gen.ImportScanItem, 0, len(result.Items)), Exhausted: result.Exhausted}
	for _, item := range result.Items {
		entry := gen.ImportScanItem{
			MessageId: item.MessageID, Name: item.Name, Size: item.Size, ModTime: item.ModTime,
			Imported: item.Imported, Link: item.Link,
		}
		if item.MimeType != "" {
			entry.MimeType = gen.NewOptString(item.MimeType)
		}
		response.Items = append(response.Items, entry)
	}
	if !result.Exhausted && result.NextID > 0 {
		response.NextCursor = gen.NewOptString(strconv.FormatInt(result.NextID, 10))
	}
	return &response, nil
}

func (h *Handler) CreateChannelImport(ctx context.Context, req *gen.ImportRequest) (gen.CreateChannelImportRes, error) {
	if h == nil || h.Importer == nil || h.Jobs == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if req == nil || req.ChannelId == 0 || len(req.MessageIds) == 0 {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_import", "channelId and at least one messageId are required", nil)
	}
	var parentID *uuid.UUID
	if value, ok := req.ParentId.Get(); ok {
		id := googleUUID(value)
		if id == uuid.Nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid_import", "parentId must be a folder id", nil)
		}
		parentID = &id
	}
	job, err := h.Importer.Create(ctx, importer.CreateInput{
		UserID: userID, ChannelID: req.ChannelId, ParentID: parentID, MessageIDs: req.MessageIds,
	})
	if err != nil {
		return nil, mapImportError(err)
	}
	if job.Total > 0 {
		jobID, ok := dbtypes.GoogleUUID(job.ID)
		if !ok {
			return nil, mapServiceError(ErrOperationUnavailable)
		}
		parentText := ""
		if parentID != nil {
			parentText = parentID.String()
		}
		if _, err := h.Jobs.InsertChannelImport(ctx, jobs.ChannelImportArgs{
			JobID: jobID.String(), UserID: userID, ChannelID: req.ChannelId, ParentID: parentText,
		}); err != nil {
			return nil, mapServiceError(err)
		}
	}
	response := importJobResponse(job)
	return &response, nil
}

func mapImportError(err error) error {
	return mapServiceError(err)
}

func importJobResponse(job *sqlcgen.ImportJob) gen.ImportJob {
	response := gen.ImportJob{
		ChannelId: job.ChannelID, Status: gen.ImportJobStatus(job.Status),
		Total: job.Total, Imported: job.Imported, Skipped: job.Skipped, Failed: job.Failed,
		CreatedAt: job.CreatedAt.Time, UpdatedAt: job.UpdatedAt.Time,
	}
	if id, ok := dbtypes.GoogleUUID(job.ID); ok {
		response.ID = apiUUID(id)
	}
	if parentID, ok := dbtypes.GoogleUUID(job.ParentID); ok {
		response.ParentId = gen.NewOptUUID(apiUUID(parentID))
	}
	if job.Error.Valid {
		response.Error = gen.NewOptString(job.Error.String)
	}
	return response
}
