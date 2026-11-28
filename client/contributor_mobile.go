//go:build ios || android

package main

import (
	"context"

	"github.com/ourchive/ourchive/internal/clientcore"
)

const isMobile = true

// Mobile operating systems suspend background apps, so a phone cannot keep a contributor tunnel
// open. Mobile builds are members-only clients: browse, upload, download and share.
type contributor struct {
	agent  clientcore.Agent
	enroll clientcore.EnrollFunc
	note   string
}

func startContributor() *contributor {
	return &contributor{note: "Phones and tablets can browse and share, but cannot contribute storage."}
}

func (c *contributor) run(context.Context) {}
func (c *contributor) stop()               {}
func (c *contributor) retryAll()           {}
