package clientcore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrContributorUnsupported is returned on builds that cannot run the contributor agent.
var ErrContributorUnsupported = errors.New("contributing storage is not supported on this device")

// queueEnrollment records an ourchive://enroll link for the user to confirm in the app. It never
// enrolls by itself: a hub that this machine contributes to can ask for any local folder to be
// shared, so only the user, looking at the hub's address inside the app, may agree to that.
func (c *Core) queueEnrollment(hubURL, token, name string) error {
	if c.opts.Agent == nil || c.opts.Enroll == nil {
		return ErrContributorUnsupported
	}
	norm, err := NormalizeHubURL(hubURL)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 512 {
		return errors.New("enrollment link has no valid token")
	}
	c.mu.Lock()
	c.enrollment = &pendingEnrollment{HubURL: norm, Token: token, Name: strings.TrimSpace(name), ReceivedAt: time.Now()}
	c.mu.Unlock()
	c.changed()
	c.shell.ShowWindow()
	c.shell.Notify("Confirm contribution", "Review the request to contribute this computer to "+norm+".")
	return nil
}

// ConfirmEnrollment accepts or dismisses the pending ourchive://enroll request.
func (c *Core) ConfirmEnrollment(ctx context.Context, accept bool, name string, readOnly bool) error {
	c.mu.Lock()
	p := c.enrollment
	c.enrollment = nil
	c.mu.Unlock()
	defer c.changed()
	if p == nil || p.expired(time.Now()) {
		return errors.New("there is no pending contribution request")
	}
	if !accept {
		return nil
	}
	hub, err := c.hubs.Add("", p.HubURL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		name = p.Name
	}
	if err := c.enroll(ctx, hub, p.Token, name, readOnly); err != nil {
		return err
	}
	c.probeHubAsync(hub)
	return nil
}

// EnrollActiveHub contributes this machine to the active hub with a token the UI just minted.
// The hub is always the active one, never an address supplied by the page.
func (c *Core) EnrollActiveHub(ctx context.Context, token, name string, readOnly bool) error {
	hub, ok := c.hubs.Active()
	if !ok {
		return errors.New("no hub selected")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("enrollment token is required")
	}
	return c.enroll(ctx, hub, strings.TrimSpace(token), name, readOnly)
}

func (c *Core) enroll(ctx context.Context, hub Hub, token, name string, readOnly bool) error {
	if c.opts.Agent == nil || c.opts.Enroll == nil {
		return ErrContributorUnsupported
	}
	path, err := c.opts.Enroll(ctx, EnrollRequest{HubURL: hub.URL, Token: token, Name: strings.TrimSpace(name), ReadOnly: readOnly})
	if err != nil {
		return err
	}
	if err := c.opts.Agent.AddHub(path); err != nil {
		return fmt.Errorf("enrolled, but could not start contributing: %w", err)
	}
	c.changed()
	c.shell.Notify("Contributing to "+hub.Name, "This computer is now connected to "+hub.Name+".")
	return nil
}

// RetryAgent asks the agent to reconnect one hub (or all hubs when hubURL is empty).
func (c *Core) RetryAgent(hubURL string) bool {
	if c.opts.Agent == nil {
		return false
	}
	if hubURL == "" {
		ok := false
		for _, h := range c.opts.Agent.Status() {
			ok = c.opts.Agent.Retry(h.HubURL) || ok
		}
		return ok
	}
	return c.opts.Agent.Retry(hubURL)
}
