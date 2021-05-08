package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/openid"
	"github.com/ory/fosite/token/jwt"

	"github.com/authelia/authelia/internal/logging"
	"github.com/authelia/authelia/internal/middlewares"
	"github.com/authelia/authelia/internal/oidc"
	"github.com/authelia/authelia/internal/session"
)

func oidcAuthorize(ctx *middlewares.AutheliaCtx, rw http.ResponseWriter, r *http.Request) {
	ar, err := ctx.Providers.OpenIDConnect.Fosite.NewAuthorizeRequest(ctx, r)
	if err != nil {
		logging.Logger().Errorf("Error occurred in NewAuthorizeRequest: %+v", err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	clientID := ar.GetClient().GetID()
	client, err := ctx.Providers.OpenIDConnect.Store.GetInternalClient(clientID)

	if err != nil {
		err := fmt.Errorf("Unable to find related client configuration with name '%s': %v", ar.GetID(), err)
		ctx.Logger.Error(err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	userSession := ctx.GetSession()

	requestedScopes := ar.GetRequestedScopes()
	requestedAudience := ar.GetRequestedAudience()

	isAuthInsufficient := !client.IsAuthenticationLevelSufficient(userSession.AuthenticationLevel)

	if isAuthInsufficient || (isConsentMissing(userSession.OIDCWorkflowSession, requestedScopes, requestedAudience)) {
		oidcAuthorizeHandleAuthorizationOrConsentInsufficient(ctx, userSession, client, isAuthInsufficient, rw, r, ar)

		return
	}

	for _, scope := range requestedScopes {
		ar.GrantScope(scope)
	}

	for _, a := range requestedAudience {
		ar.GrantAudience(a)
	}

	userSession.OIDCWorkflowSession = nil
	if err := ctx.SaveSession(userSession); err != nil {
		ctx.Logger.Errorf("%v", err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)

		return
	}

	claims := &jwt.IDTokenClaims{
		Subject:     userSession.Username,
		Issuer:      ctx.Configuration.ExternalURL,
		AuthTime:    time.Unix(userSession.Authenticated, 0),
		RequestedAt: userSession.OIDCWorkflowSession.Created,
		IssuedAt:    time.Now(),
		Nonce:       ar.GetRequestForm().Get("nonce"),
		Audience:    []string{ar.GetClient().GetID()},
	}

	response, err := ctx.Providers.OpenIDConnect.Fosite.NewAuthorizeResponse(ctx, ar, &oidc.OpenIDSession{
		DefaultSession: &openid.DefaultSession{
			Claims: claims,
			Headers: &jwt.Headers{Extra: map[string]interface{}{
				"kid": ctx.Providers.OpenIDConnect.Store.KeyManager.GetActiveKeyID(),
			}},
			Subject: userSession.Username,
		},
		ClientID: clientID,
	})
	if err != nil {
		ctx.Logger.Errorf("Error occurred in NewAuthorizeResponse: %+v", err)
		ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeError(rw, ar, err)

		return
	}

	ctx.Providers.OpenIDConnect.Fosite.WriteAuthorizeResponse(rw, ar, response)
}

func oidcAuthorizeHandleAuthorizationOrConsentInsufficient(
	ctx *middlewares.AutheliaCtx, userSession session.UserSession, client *oidc.InternalClient, isAuthInsufficient bool,
	rw http.ResponseWriter, r *http.Request,
	ar fosite.AuthorizeRequester) {
	forwardedProtoHost, err := ctx.ForwardedProtoHost()
	if err != nil {
		ctx.Logger.Errorf("%v", err)
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	redirectURL := fmt.Sprintf("%s%s", forwardedProtoHost, string(ctx.Request.RequestURI()))

	ctx.Logger.Debugf("User %s must consent with scopes %s",
		userSession.Username, strings.Join(ar.GetRequestedScopes(), ", "))

	userSession.OIDCWorkflowSession = &session.OIDCWorkflowSession{
		ClientID:                   client.ID,
		RequestedScopes:            ar.GetRequestedScopes(),
		RequestedAudience:          ar.GetRequestedAudience(),
		AuthURI:                    redirectURL,
		TargetURI:                  ar.GetRedirectURI().String(),
		RequiredAuthorizationLevel: client.Policy,
		Created:                    time.Now(),
	}

	if err := ctx.SaveSession(userSession); err != nil {
		ctx.Logger.Errorf("%v", err)
		http.Error(rw, err.Error(), http.StatusInternalServerError)

		return
	}

	uri, err := ctx.ForwardedProtoHost()
	if err != nil {
		ctx.Logger.Errorf("%v", err)
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	if isAuthInsufficient {
		http.Redirect(rw, r, uri, http.StatusFound)
	} else {
		http.Redirect(rw, r, fmt.Sprintf("%s/consent", uri), http.StatusFound)
	}
}