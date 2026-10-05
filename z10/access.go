package z10

import (
	"context"
	"net/http"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
)

const (
	// AccessProviderType is the sdkaccess registry key and provider identifier.
	AccessProviderType = "z10-friend-keys"

	authErrorCodeInactive sdkaccess.AuthErrorCode = "friend_key_inactive"
)

// accessProvider authenticates friend keys. It answers NotHandled for every other
// credential so owner keys keep flowing through the built-in config provider.
type accessProvider struct {
	rt *Runtime
}

func (p *accessProvider) Identifier() string { return AccessProviderType }

func (p *accessProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	friend, source := p.rt.Friends().match(r)
	if friend == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if reason := friend.inactiveReason(p.rt.now()); reason != "" {
		// A non-continuing code stops the manager from reporting a generic error.
		return nil, &sdkaccess.AuthError{Code: authErrorCodeInactive, Message: reason, StatusCode: http.StatusUnauthorized}
	}
	return &sdkaccess.Result{
		Provider:  AccessProviderType,
		Principal: PrincipalPrefix + friend.Name,
		Metadata:  map[string]string{"source": source, "friend": friend.Name},
	}, nil
}
