package watch

import (
	"context"
	"fmt"
	"time"

	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/mail"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

// EmailNotifier sends notices through the email integration, reading it
// fresh each time so a change takes effect without a restart.
type EmailNotifier struct {
	Secrets *secrets.Store
	Store   *state.Store
}

// Notify implements Notifier.
func (e EmailNotifier) Notify(ctx context.Context, n Notice) error {
	cfg, err := integration.LoadEmail(e.Secrets)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err = mail.Send(ctx, mail.Server{Host: cfg.Host, Port: cfg.Port, User: cfg.User, Password: cfg.Password},
		mail.Message{From: cfg.From, To: cfg.To, Subject: "[bedrock] " + n.Subject, Body: n.Body})
	if err != nil {
		return err
	}
	if e.Store != nil {
		_ = e.Store.RecordIntegrationUse(ctx, integration.EmailName, "alert: "+n.Subject, time.Now().UTC())
	}
	return nil
}

// Test sends a test message through the email integration.
func (e EmailNotifier) Test(ctx context.Context, hostname string) error {
	return e.Notify(ctx, Notice{Subject: hostname + ": test alert", Body: fmt.Sprintf("This is bedrock on %s checking that alerts reach you. Nothing is wrong.\n", hostname)})
}
