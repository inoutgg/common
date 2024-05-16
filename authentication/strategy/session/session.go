package session

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"go.inout.gg/common/authentication/db/driver"
	"go.inout.gg/common/authentication/strategy"
	"go.inout.gg/common/authentication/user"
	"go.inout.gg/common/http/cookie"
	httperror "go.inout.gg/common/http/error"
	"go.inout.gg/common/sql/dbutil"
)

var _ strategy.Authenticator[any] = (*session[any])(nil)

var (
	CookieName = "usid"
)

type session[T any] struct {
	driver driver.Driver
	config *Config
}

type Config struct {
	CookieName string
}

func New[T any](driver driver.Driver, config *Config) strategy.Authenticator[T] {
	return &session[T]{driver, config}
}

func (s *session[T]) Authenticate(
	w http.ResponseWriter,
	r *http.Request,
) (*strategy.User[T], error) {
	ctx := r.Context()
	val := cookie.Get(r, CookieName)
	if val == "" {
		return nil, user.ErrUnauthorizedUser
	}

	val, err := s.decodeSession(val)
	if err != nil {
		return nil, fmt.Errorf("authentication/session: failed to decode session: %w", err)
	}

	tx, err := s.driver.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("authentication/session: failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	q := tx.Queries()
	_, err = q.FindUserSessionByID(ctx, uuid.UUID{})
	if err != nil {
		if dbutil.IsNotFoundError(err) {
			return nil, user.ErrUnauthorizedUser
		}

		return nil, fmt.Errorf("authentication/session: failed to find user session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("authentication/session: failed to commit transaction: %w", err)
	}

	return nil, nil
}

func (s *session[T]) decodeSession(val string) (string, error) {
	bytes, err := base64.URLEncoding.DecodeString(val)
	if err != nil {
		return "", fmt.Errorf("authentication/session: failed to decode cookie: %w", err)
	}

	return string(bytes), nil
}

func (s *session[T]) LogOut(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	q := s.driver.Queries()

	usr := user.FromRequest[any](r)
	if usr == nil {
		return httperror.FromError(user.ErrUnauthorizedUser, http.StatusUnauthorized)
	}

	if _, err := q.ExpireSessionByID(ctx, usr.ID); err != nil {
		return httperror.FromError(err, http.StatusInternalServerError)
	}

	// Delete session cookie.
	cookie.Delete(w, r, s.config.CookieName)

	return nil
}