// Package info caches the CLI version and account for /api/machine. Nothing
// here runs per request: the CLI is probed once at startup and every 30
// minutes.
package info

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"even-cc/internal/claude"
	"even-cc/internal/guard"
)

// Account is ordered like even-terminal's response.
type Account struct {
	Email            string `json:"email"`
	Organization     string `json:"organization"`
	SubscriptionType string `json:"subscriptionType"`
}

type Response struct {
	Account any    `json:"account"` // *Account or {}
	Version string `json:"version"`
}

type Info struct {
	claudePath string
	env        []string
	log        *slog.Logger

	ready chan struct{}
	once  sync.Once

	mu      sync.Mutex
	version string
	account *Account
}

func New(claudePath string, env []string, log *slog.Logger) *Info {
	return &Info{claudePath: claudePath, env: env, log: log, ready: make(chan struct{})}
}

// Start probes the CLI now and then every 30 minutes.
func (i *Info) Start(ctx context.Context) {
	safeProbe := func() {
		defer guard.Recover(i.log, "claude probe")
		i.probe(ctx)
	}
	go func() {
		defer guard.Recover(i.log, "claude probe loop")
		safeProbe()
		i.once.Do(func() { close(i.ready) })
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				safeProbe()
			}
		}
	}()
}

func (i *Info) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, i.claudePath, args...)
	cmd.Env = claude.ChildEnv(i.env)
	cmd.WaitDelay = 2 * time.Second // a child keeping stdout open can't hold Output past the timeout
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (i *Info) probe(ctx context.Context) {
	if i.claudePath == "" {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer guard.Recover(i.log, "claude --version")
		out, err := i.run(ctx, 5*time.Second, "--version")
		if err != nil {
			i.log.Warn("claude --version failed", "err", err)
			return
		}
		v := strings.TrimSpace(strings.Replace(out, " (Claude Code)", "", 1))
		i.mu.Lock()
		i.version = v
		i.mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		defer guard.Recover(i.log, "claude auth status")
		out, err := i.run(ctx, 8*time.Second, "auth", "status")
		if err != nil {
			i.log.Warn("claude auth status failed", "err", err)
			return
		}
		var a struct {
			Email            *string `json:"email"`
			OrgName          *string `json:"orgName"`
			SubscriptionType *string `json:"subscriptionType"`
		}
		if json.Unmarshal([]byte(out), &a) != nil {
			return
		}
		acc := &Account{}
		if a.Email != nil {
			acc.Email = *a.Email
		}
		if a.OrgName != nil {
			acc.Organization = *a.OrgName
		}
		if a.SubscriptionType != nil {
			acc.SubscriptionType = *a.SubscriptionType
		}
		i.mu.Lock()
		i.account = acc
		i.mu.Unlock()
	}()
	wg.Wait()
	i.log.Info("claude info", "version", i.Version())
}

// Version returns the cached CLI version ("" until probed).
func (i *Info) Version() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.version
}

// FromInitialize fills gaps from a process's initialize response.
func (i *Info) FromInitialize(raw json.RawMessage) {
	var r struct {
		Version string `json:"claude_code_version"`
		Account *struct {
			Email            string `json:"email"`
			Organization     string `json:"organization"`
			SubscriptionType string `json:"subscriptionType"`
		} `json:"account"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if r.Version != "" {
		i.version = r.Version
	}
	if i.account == nil && r.Account != nil {
		i.account = &Account{Email: r.Account.Email, Organization: r.Account.Organization, SubscriptionType: r.Account.SubscriptionType}
	}
}

// Get returns the response, waiting up to 6 s for the first probe.
func (i *Info) Get() Response {
	select {
	case <-i.ready:
	case <-time.After(6 * time.Second):
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	r := Response{Version: i.version}
	if r.Version == "" {
		r.Version = "Unknown"
	}
	if i.account != nil {
		r.Account = i.account
	} else {
		r.Account = struct{}{}
	}
	return r
}
