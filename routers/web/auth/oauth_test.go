// Copyright 2021 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/services/oauth2_provider"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

func createAndParseToken(t *testing.T, grant *auth.OAuth2Grant) *oauth2_provider.OIDCToken {
	signingKey, err := oauth2_provider.CreateJWTSigningKey("HS256", make([]byte, 32))
	assert.NoError(t, err)
	assert.NotNil(t, signingKey)

	response, terr := oauth2_provider.NewAccessTokenResponse(t.Context(), grant, signingKey, signingKey)
	assert.Nil(t, terr)
	assert.NotNil(t, response)

	parsedToken, err := jwt.ParseWithClaims(response.IDToken, &oauth2_provider.OIDCToken{}, func(token *jwt.Token) (any, error) {
		assert.NotNil(t, token.Method)
		assert.Equal(t, signingKey.SigningMethod().Alg(), token.Method.Alg())
		return signingKey.VerifyKey(), nil
	})
	assert.NoError(t, err)
	assert.True(t, parsedToken.Valid)

	oidcToken, ok := parsedToken.Claims.(*oauth2_provider.OIDCToken)
	assert.True(t, ok)
	assert.NotNil(t, oidcToken)

	return oidcToken
}

func TestAvatarErrForLog(t *testing.T) {
	// A real request to a closed port produces the *url.Error that http.Client
	// returns in production; its Error() embeds the whole request URL.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoError(t, err)
	closedAddr := listener.Addr().String()
	assert.NoError(t, listener.Close())

	const secret = "s3cret-signature-value"
	avatarURL := "http://" + closedAddr + "/avatar.png?token=" + secret

	req, err := http.NewRequest(http.MethodGet, avatarURL, nil)
	assert.NoError(t, err)
	_, err = (&http.Client{Timeout: 5 * time.Second}).Do(req)
	assert.Error(t, err)

	// Guard against the test going vacuous: the raw error must leak the secret,
	// otherwise it is not exercising the case this sanitiser exists for.
	assert.Contains(t, err.Error(), secret)

	sanitised := avatarErrForLog(err).Error()
	assert.NotContains(t, sanitised, secret)
	assert.NotContains(t, sanitised, "?")
	assert.NotEmpty(t, sanitised)

	// Errors that do not wrap a URL are passed through untouched.
	plain := errors.New("some unrelated failure")
	assert.Equal(t, plain, avatarErrForLog(plain))
}

func TestNewAccessTokenResponse_OIDCToken(t *testing.T) {
	assert.NoError(t, unittest.PrepareTestDatabase())

	grants, err := auth.GetOAuth2GrantsByUserID(t.Context(), 3)
	assert.NoError(t, err)
	assert.Len(t, grants, 1)

	// Scopes: openid
	oidcToken := createAndParseToken(t, grants[0])
	assert.Empty(t, oidcToken.Name)
	assert.Empty(t, oidcToken.PreferredUsername)
	assert.Empty(t, oidcToken.Profile)
	assert.Empty(t, oidcToken.Picture)
	assert.Empty(t, oidcToken.Website)
	assert.Empty(t, oidcToken.UpdatedAt)
	assert.Empty(t, oidcToken.Email)
	assert.False(t, oidcToken.EmailVerified)

	user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 5})
	grants, err = auth.GetOAuth2GrantsByUserID(t.Context(), user.ID)
	assert.NoError(t, err)
	assert.Len(t, grants, 1)

	// Scopes: openid profile email
	oidcToken = createAndParseToken(t, grants[0])
	assert.Equal(t, user.DisplayName(), oidcToken.Name)
	assert.Equal(t, user.Name, oidcToken.PreferredUsername)
	assert.Equal(t, user.HTMLURL(t.Context()), oidcToken.Profile)
	assert.Equal(t, user.AvatarLink(t.Context()), oidcToken.Picture)
	assert.Equal(t, user.Website, oidcToken.Website)
	assert.Equal(t, user.UpdatedUnix, oidcToken.UpdatedAt)
	assert.Equal(t, user.Email, oidcToken.Email)
	assert.Equal(t, user.IsActive, oidcToken.EmailVerified)
}
