package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/webdav"
)

func (h *Handler) GetWebDAVCredentials(ctx context.Context) (gen.GetWebDAVCredentialsRes, error) {
	if h == nil || h.WebDAVCredentials == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	credential, err := h.WebDAVCredentials.GetByUser(ctx, userID)
	if err != nil {
		return nil, mapServiceError(err)
	}
	response := gen.WebDAVCredentials{Enabled: h.WebDAVEnabled}
	if credential != nil {
		response.Configured = true
		response.Username = gen.NewOptString(credential.Username)
	}
	if h.WebDAVPrefix != "" {
		response.URL = gen.NewOptString(h.WebDAVPrefix)
	}
	return &response, nil
}

func (h *Handler) SetWebDAVCredentials(ctx context.Context, req *gen.WebDAVCredentialsUpdate) (gen.SetWebDAVCredentialsRes, error) {
	if h == nil || h.WebDAVCredentials == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if req == nil || strings.TrimSpace(req.Username) == "" || len(req.Password) < 8 {
		return nil, problem(http.StatusUnprocessableEntity, "invalid_webdav_credentials", "username and a password of at least 8 characters are required", nil)
	}
	credential, err := h.WebDAVCredentials.Set(ctx, userID, req.Username, req.Password)
	if err != nil {
		if errors.Is(err, webdav.ErrCredentialConflict) {
			return nil, problem(http.StatusConflict, "webdav_username_taken", "that WebDAV username is already in use", err)
		}
		return nil, mapServiceError(err)
	}
	response := gen.WebDAVCredentials{
		Enabled: h.WebDAVEnabled, Configured: true, Username: gen.NewOptString(credential.Username),
	}
	if h.WebDAVPrefix != "" {
		response.URL = gen.NewOptString(h.WebDAVPrefix)
	}
	return &response, nil
}

func (h *Handler) DeleteWebDAVCredentials(ctx context.Context) (gen.DeleteWebDAVCredentialsRes, error) {
	if h == nil || h.WebDAVCredentials == nil {
		return nil, mapServiceError(ErrOperationUnavailable)
	}
	userID, err := UserIDFromContext(ctx)
	if err != nil {
		return nil, mapServiceError(err)
	}
	if err := h.WebDAVCredentials.Delete(ctx, userID); err != nil {
		return nil, mapServiceError(err)
	}
	return &gen.DeleteWebDAVCredentialsNoContent{}, nil
}
