package api

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/tgdrive/teldrive/v2/internal/api/gen"
	"github.com/tgdrive/teldrive/v2/internal/principal"
	"github.com/tgdrive/teldrive/v2/internal/streamtoken"
)

var (
	ErrUnauthenticated = errors.New("authentication failed")
	ErrInvalidIdentity = errors.New("authenticated identity is invalid")
)

type Identity = principal.Identity

type Authenticator interface {
	AuthenticateBearer(context.Context, string) (Identity, error)
	AuthenticateAPIKey(context.Context, string) (Identity, error)
}

type EventTicketAuthenticator interface {
	AuthenticateTicket(context.Context, string) (int64, error)
}

type Security struct {
	authenticator Authenticator
	eventTickets  EventTicketAuthenticator
	streamTokens  *streamtoken.Signer
}

// SetStreamTokenSigner enables signed media player URLs for file content
// operations. The generated security handler only offers this scheme on those
// operations.
func (s *Security) SetStreamTokenSigner(signer *streamtoken.Signer) *Security {
	if s != nil {
		s.streamTokens = signer
	}
	return s
}

func NewSecurity(authenticator Authenticator, eventTickets ...EventTicketAuthenticator) *Security {
	security := &Security{authenticator: authenticator}
	if len(eventTickets) > 0 {
		security.eventTickets = eventTickets[0]
	}
	return security
}

func IdentityFromContext(ctx context.Context) (Identity, bool) {
	return principal.FromContext(ctx)
}

func UserIDFromContext(ctx context.Context) (int64, error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return 0, ErrUnauthenticated
	}
	return identity.UserID, nil
}

func HasRole(ctx context.Context, role string) bool {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		return false
	}
	return slices.Contains(identity.Roles, role)
}

func HasAdminRole(ctx context.Context) bool {
	return HasRole(ctx, "admin") || HasRole(ctx, "owner")
}

func (s *Security) HandleBearerAuth(ctx context.Context, _ gen.OperationName, auth gen.BearerAuth) (context.Context, error) {
	if s == nil || s.authenticator == nil || strings.TrimSpace(auth.Token) == "" {
		return ctx, ErrUnauthenticated
	}
	identity, err := s.authenticator.AuthenticateBearer(ctx, auth.Token)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if identity.UserID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	auth.Roles = append([]string(nil), identity.Roles...)
	return principal.WithIdentity(ctx, identity), nil
}

func (s *Security) HandleExternalApiKeyAuth(ctx context.Context, _ gen.OperationName, auth gen.ExternalApiKeyAuth) (context.Context, error) {
	if s == nil || s.authenticator == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	identity, err := s.authenticator.AuthenticateAPIKey(ctx, auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if identity.UserID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	auth.Roles = append([]string(nil), identity.Roles...)
	return principal.WithIdentity(ctx, identity), nil
}

func (s *Security) HandleCookieAuth(ctx context.Context, _ gen.OperationName, auth gen.CookieAuth) (context.Context, error) {
	if s == nil || s.authenticator == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	identity, err := s.authenticator.AuthenticateBearer(ctx, auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if identity.UserID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	auth.Roles = append([]string(nil), identity.Roles...)
	return principal.WithIdentity(ctx, identity), nil
}

func (s *Security) HandleEventTicketAuth(ctx context.Context, _ gen.OperationName, auth gen.EventTicketAuth) (context.Context, error) {
	if s == nil || s.eventTickets == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	userID, err := s.eventTickets.AuthenticateTicket(ctx, auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	if userID <= 0 {
		return ctx, ErrInvalidIdentity
	}
	return principal.WithIdentity(ctx, principal.Identity{UserID: userID, Source: "event_ticket"}), nil
}

func (s *Security) HandleStreamTokenAuth(ctx context.Context, _ gen.OperationName, auth gen.StreamTokenAuth) (context.Context, error) {
	if s == nil || s.streamTokens == nil || strings.TrimSpace(auth.APIKey) == "" {
		return ctx, ErrUnauthenticated
	}
	claims, err := s.streamTokens.Verify(auth.APIKey)
	if err != nil {
		return ctx, errors.Join(ErrUnauthenticated, err)
	}
	auth.Roles = append([]string(nil), auth.Roles...)
	ctx = principal.WithIdentity(ctx, principal.Identity{UserID: claims.UserID, Source: "stream_token"})
	return streamtoken.WithClaims(ctx, claims), nil
}

var _ gen.SecurityHandler = (*Security)(nil)
