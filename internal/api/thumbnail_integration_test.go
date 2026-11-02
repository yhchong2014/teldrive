//go:build integration

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	api "github.com/tgdrive/teldrive/v2/internal/api"
	"github.com/tgdrive/teldrive/v2/internal/catalog"
	"github.com/tgdrive/teldrive/v2/internal/shares"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
	testpostgres "github.com/tgdrive/teldrive/v2/internal/testutil/postgres"
)

type fakeThumbnailReader struct {
	requests []telegramstore.MetadataRequest
	err      error
}

func (r *fakeThumbnailReader) Thumbnail(_ context.Context, request telegramstore.MetadataRequest) ([]byte, string, error) {
	r.requests = append(r.requests, request)
	return []byte{0xff, 0xd8, 0xff, 0xd9}, "image/jpeg", r.err
}

func TestThumbnailRequiresFileAccessAndSkipsEncrypted(t *testing.T) {
	db := testpostgres.New(t)
	ctx := context.Background()
	folder, video, encrypted := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO users (user_id) VALUES (2001), (2002)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO files (id,user_id,parent_id,name,kind,mime_type,size,encryption,encryption_key_version,mod_time) VALUES
($1,2001,NULL,'folder','folder',NULL,NULL,false,NULL,now()),
($2,2001,$1,'video.mp4','file','video/mp4',10,false,NULL,now()),
($3,2001,$1,'encrypted.mp4','file','video/mp4',10,true,1,now())`, folder, video, encrypted); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO file_parts (file_id,part_no,channel_id,message_id,plain_size,stored_size) VALUES ($1,1,55,5,10,10), ($2,1,55,6,10,10)`, video, encrypted); err != nil {
		t.Fatal(err)
	}
	catalogService := catalog.NewService(db.Pool, nil)
	shareService, err := shares.NewService(db.Pool, catalogService)
	if err != nil {
		t.Fatal(err)
	}
	reader := &fakeThumbnailReader{}
	handler := api.NewHandler(catalogService, nil, nil, nil, nil, 0, nil).ConfigureDomains(nil, nil, nil, nil, shareService, nil)
	handler.Thumbnails = reader
	server, err := api.NewServer(handler, api.NewSecurity(listingAuthenticator{}))
	if err != nil {
		t.Fatal(err)
	}
	request := func(id uuid.UUID, token string) int {
		headers := map[string]string{}
		if token != "" {
			headers["Authorization"] = "Bearer " + token
		}
		return performRequest(t, server, http.MethodGet, "/v1/files/"+id.String()+"/thumbnail", nil, headers).Code
	}
	if got := request(video, ""); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", got)
	}
	if got := request(video, "user-2002"); got != http.StatusForbidden {
		t.Fatalf("ungranted status = %d", got)
	}
	if len(reader.requests) != 0 {
		t.Fatal("unauthorized request reached Telegram")
	}
	if got := request(encrypted, "user-2001"); got != http.StatusNotFound {
		t.Fatalf("encrypted status = %d", got)
	}
	if len(reader.requests) != 0 {
		t.Fatal("encrypted request reached Telegram")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO file_access_grants (file_id,owner_id,grantee_id,permission) VALUES ($1,2001,2002,'read')`, folder); err != nil {
		t.Fatal(err)
	}
	granted := performRequest(t, server, http.MethodGet, "/v1/files/"+video.String()+"/thumbnail", nil, map[string]string{"Authorization": "Bearer user-2002"})
	if granted.Code != http.StatusOK {
		t.Fatalf("granted status = %d", granted.Code)
	}
	if granted.Header().Get("Cache-Control") != "private, no-store" || granted.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe thumbnail headers: %v", granted.Header())
	}
	if len(reader.requests) != 1 || reader.requests[0] != (telegramstore.MetadataRequest{UserID: 2001, ChannelID: 55, MessageID: 5}) {
		t.Fatalf("thumbnail requests = %+v", reader.requests)
	}
	reader.err = telegramstore.ErrThumbnailNotFound
	if got := request(video, "user-2001"); got != http.StatusNotFound {
		t.Fatalf("missing thumbnail status = %d", got)
	}
}
