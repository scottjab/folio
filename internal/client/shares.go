package client

import (
	"context"
	"net/http"
)

// Shares lists the grants the caller has handed out.
func (c *Client) Shares(ctx context.Context) ([]Share, error) {
	out, _, err := c.do[struct {
		Shares []Share `json:"shares"`
	}](ctx, request{Method: http.MethodGet, Path: "/api/shares"})
	return out.Shares, err
}

// SharedWithMe lists the grants other people have made to the caller.
func (c *Client) SharedWithMe(ctx context.Context) ([]Share, error) {
	out, _, err := c.do[struct {
		Shares []Share `json:"shares"`
	}](ctx, request{Method: http.MethodGet, Path: "/api/shared"})
	return out.Shares, err
}

// Grant gives another tailnet user access to a note or folder in the caller's
// own vault. There is no vault parameter because the server only ever grants
// out of the caller's vault.
func (c *Client) Grant(ctx context.Context, path string, isFolder bool, grantee string, perm Perm) (Share, error) {
	body := struct {
		Path     string `json:"path"`
		Grantee  string `json:"grantee"`
		Perm     Perm   `json:"perm"`
		IsFolder bool   `json:"isFolder"`
	}{path, grantee, perm, isFolder}
	s, _, err := c.do[Share](ctx, request{Method: http.MethodPost, Path: "/api/shares", Body: body})
	return s, err
}

// Revoke removes a grant the caller made.
func (c *Client) Revoke(ctx context.Context, shareID string) error {
	_, _, err := c.do[struct{}](ctx, request{
		Method: http.MethodDelete, Path: "/api/shares/" + shareID,
	})
	return err
}
