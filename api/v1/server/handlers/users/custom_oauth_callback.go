package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
	"golang.org/x/oauth2"

	"github.com/hatchet-dev/hatchet/api/v1/server/authn"
	"github.com/hatchet-dev/hatchet/api/v1/server/middleware/redirect"
	"github.com/hatchet-dev/hatchet/api/v1/server/oas/gen"
	"github.com/hatchet-dev/hatchet/pkg/config/server"
	"github.com/hatchet-dev/hatchet/pkg/repository"
	"github.com/hatchet-dev/hatchet/pkg/repository/prisma/db"
)

// Note: we want all errors to redirect, otherwise the user will be greeted with raw JSON in the middle of the login flow.
func (u *UserService) UserUpdateCustomOauthCallback(ctx echo.Context, _ gen.UserUpdateCustomOauthCallbackRequestObject) (gen.UserUpdateCustomOauthCallbackResponseObject, error) {
	isValid, _, err := authn.NewSessionHelpers(u.config).ValidateOAuthState(ctx, "custom")

	if err != nil || !isValid {
		return nil, redirect.GetRedirectWithError(ctx, u.config.Logger, err, "Could not log in. Please try again and make sure cookies are enabled.")
	}

	token, err := u.config.Auth.CustomOAuthConfig.Exchange(context.Background(), ctx.Request().URL.Query().Get("code"))

	if err != nil {
		return nil, redirect.GetRedirectWithError(ctx, u.config.Logger, err, "Forbidden")
	}

	if !token.Valid() {
		return nil, redirect.GetRedirectWithError(ctx, u.config.Logger, fmt.Errorf("invalid token"), "Forbidden")
	}

	user, err := u.upsertCustomUserFromToken(u.config, token)

	if err != nil {
		if errors.Is(err, ErrNotInRestrictedDomain) {
			return nil, redirect.GetRedirectWithError(ctx, u.config.Logger, err, "Email is not in the restricted domain group.")
		}

		return nil, redirect.GetRedirectWithError(ctx, u.config.Logger, err, "Internal error.")
	}

	err = authn.NewSessionHelpers(u.config).SaveAuthenticated(ctx, user)

	if err != nil {
		return nil, redirect.GetRedirectWithError(ctx, u.config.Logger, err, "Internal error.")
	}

	return gen.UserUpdateCustomOauthCallback302Response{
		Headers: gen.UserUpdateCustomOauthCallback302ResponseHeaders{
			Location: u.config.Runtime.ServerURL,
		},
	}, nil
}

func (u *UserService) upsertCustomUserFromToken(config *server.ServerConfig, tok *oauth2.Token) (*db.UserModel, error) {
	cInfo, err := getCustomUserInfoFromToken(config, tok)
	if err != nil {
		return nil, err
	}

	if err := u.checkUserRestrictions(config, cInfo.Email); err != nil {
		return nil, err
	}

	expiresAt := tok.Expiry

	// use the encryption service to encrypt the access and refresh token
	accessTokenEncrypted, err := config.Encryption.Encrypt([]byte(tok.AccessToken), "custom_access_token")

	if err != nil {
		return nil, fmt.Errorf("failed to encrypt access token: %s", err.Error())
	}

	refreshTokenEncrypted, err := config.Encryption.Encrypt([]byte(tok.RefreshToken), "custom_refresh_token")

	if err != nil {
		return nil, fmt.Errorf("failed to encrypt refresh token: %s", err.Error())
	}

	oauthOpts := &repository.OAuthOpts{
		Provider:       "custom",
		ProviderUserId: cInfo.Sub,
		AccessToken:    accessTokenEncrypted,
		RefreshToken:   &refreshTokenEncrypted,
		ExpiresAt:      &expiresAt,
	}

	user, err := u.config.APIRepository.User().GetUserByEmail(cInfo.Email)

	switch err {
	case nil:
		user, err = u.config.APIRepository.User().UpdateUser(user.ID, &repository.UpdateUserOpts{
			EmailVerified: repository.BoolPtr(cInfo.EmailVerified),
			Name:          repository.StringPtr(cInfo.Name),
			OAuth:         oauthOpts,
		})

		if err != nil {
			return nil, fmt.Errorf("failed to update user: %s", err.Error())
		}
	case db.ErrNotFound:
		user, err = u.config.APIRepository.User().CreateUser(&repository.CreateUserOpts{
			Email:         cInfo.Email,
			EmailVerified: repository.BoolPtr(cInfo.EmailVerified),
			Name:          repository.StringPtr(cInfo.Name),
			OAuth:         oauthOpts,
		})

		if err != nil {
			return nil, fmt.Errorf("failed to create user: %s", err.Error())
		}
	default:
		return nil, fmt.Errorf("failed to get user: %s", err.Error())
	}

	return user, nil
}

type customUserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func getCustomUserInfoFromToken(config *server.ServerConfig, tok *oauth2.Token) (*customUserInfo, error) {
	// use ResourceURL endpoint from the config
	url := config.Auth.ConfigFile.Custom.ResourceURL

	fmt.Printf("Response body contents: %s", config.Auth.ConfigFile.Custom.Scopes)

	req, err := http.NewRequest("GET", url, nil)

	if err != nil {
		return nil, fmt.Errorf("failed creating request: %s", err.Error())
	}

	req.Header.Add("Authorization", "Bearer "+tok.AccessToken)

	client := &http.Client{}

	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed getting user info: %s", err.Error())
	}

	defer response.Body.Close()

	contents, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %s", err.Error())
	}

	// parse contents into generic oauth2 userinfo claims
	cInfo := &customUserInfo{}
	err = json.Unmarshal(contents, &cInfo)

	if err != nil {
		return nil, fmt.Errorf("failed parsing response body: %s", err.Error())
	}

	return cInfo, nil
}