package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

type tokenReply struct {
	AccessToken  string      `json:"access_token"`
	TokenType    string      `json:"token_type"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    json.Number `json:"expires_in"`
	Error        string      `json:"error"`
}

// Refresh sends the resource indicator from RFC 8707. oauth2.Config exposes
// extra parameters for the code exchange only.
func (g oauthGrant) refreshToken(ctx context.Context, client *http.Client) (*oauth2.Token, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.Token.RefreshToken}, "resource": {g.Resource}}
	config := g.Config
	basic := config.Endpoint.AuthStyle == oauth2.AuthStyleInHeader ||
		(config.Endpoint.AuthStyle == oauth2.AuthStyleAutoDetect && config.ClientSecret != "")
	if !basic {
		form.Set("client_id", config.ClientID)
		if config.ClientSecret != "" {
			form.Set("client_secret", config.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		req.SetBasicAuth(url.QueryEscape(config.ClientID), url.QueryEscape(config.ClientSecret))
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	reply, err := readTokenReply(response)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || reply.Error != "" {
		return nil, &oauth2.RetrieveError{Response: response, ErrorCode: reply.Error}
	}
	token := &oauth2.Token{AccessToken: reply.AccessToken, TokenType: reply.TokenType, RefreshToken: reply.RefreshToken}
	if token.RefreshToken == "" {
		token.RefreshToken = g.Token.RefreshToken
	}
	if reply.ExpiresIn != "" {
		seconds, err := reply.ExpiresIn.Int64()
		if err != nil || seconds < 0 {
			return nil, errors.New("invalid MCP OAuth token lifetime")
		}
		if seconds > 0 {
			token.Expiry = time.Now().Add(time.Duration(min(seconds, math.MaxInt32)) * time.Second)
		}
	}
	return token, nil
}

func readTokenReply(response *http.Response) (tokenReply, error) {
	const maxSize = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSize+1))
	if err != nil {
		return tokenReply{}, err
	}
	if len(raw) > maxSize {
		return tokenReply{}, errors.New("MCP OAuth token response is too large")
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" || mediaType == "text/plain" {
		form, err := url.ParseQuery(string(raw))
		return tokenReply{
			AccessToken: form.Get("access_token"), TokenType: form.Get("token_type"), RefreshToken: form.Get("refresh_token"),
			ExpiresIn: json.Number(form.Get("expires_in")), Error: form.Get("error"),
		}, err
	}
	var reply tokenReply
	err = json.Unmarshal(raw, &reply)
	return reply, err
}
