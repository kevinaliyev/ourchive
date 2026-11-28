//go:build !ios && !android

package main

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/ourchive/ourchive/internal/agentcore"
	"github.com/ourchive/ourchive/internal/clientcore"
)

const isMobile = false

// contributor owns the embedded agent on desktop platforms.
type contributor struct {
	agent  clientcore.Agent
	enroll clientcore.EnrollFunc
	note   string

	runner   *agentcore.Runner
	lock     *agentcore.InstanceLock
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// startContributor takes the per-user agent lock and loads enrolled hubs. If the headless CLI
// agent already runs on this machine, the app still works as a client but leaves contributing to
// that process, and says so in the UI.
func startContributor() *contributor {
	lock, err := agentcore.AcquireInstanceLock("Ourchive desktop app")
	if err != nil {
		note := "Contributing is unavailable: " + err.Error()
		if errors.Is(err, agentcore.ErrAlreadyRunning) {
			note = "This computer is already contributing through the ourchive-agent service. " +
				"Run `ourchive-agent service uninstall`, stop it, and restart the app to manage contributing here."
		}
		log.Print(note)
		return &contributor{note: note}
	}
	runner := agentcore.New(agentcore.Options{MaxRetries: 5})
	return &contributor{
		agent:  runner,
		runner: runner,
		lock:   lock,
		enroll: func(ctx context.Context, req clientcore.EnrollRequest) (string, error) {
			res, err := agentcore.Enroll(ctx, agentcore.EnrollOptions{
				HubURL:   req.HubURL,
				Token:    req.Token,
				Name:     req.Name,
				ReadOnly: req.ReadOnly,
			})
			if err != nil {
				return "", err
			}
			return res.ConfigPath, nil
		},
	}
}

// run starts the agent in the background.
func (c *contributor) run(ctx context.Context) {
	if c.runner == nil {
		return
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		_ = c.runner.Run(ctx)
	}()
}

// stop disconnects every hub and releases the agent lock.
func (c *contributor) stop() {
	c.stopOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
			<-c.done
		}
		c.lock.Release()
	})
}

// retryAll is used by the tray.
func (c *contributor) retryAll() {
	if c.runner != nil {
		c.runner.RetryAll()
	}
}
