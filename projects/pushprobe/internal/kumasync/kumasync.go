// Package kumasync sets up the Uptime Kuma push monitors for a file's
// checks: it creates the ones missing, keeps the ones there in step, and
// never deletes or pauses a monitor.
package kumasync

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/kuma"
)

// Action is what sync does for one check.
type Action struct {
	Check string
	Name  string // the monitor's name in Kuma
	Op    string // "create", "update" or "ok"
	ID    int64  // the monitor, for update and ok
	// Token is the push token the monitor ends up with; Var is the
	// variable it is saved as (when the file says push_token: ${VAR}).
	Token, Var string
	NewToken   bool // made by sync
	Interval   int  // seconds
	Changes    []string
}

// Plan is everything sync does.
type Plan struct {
	Group       string
	GroupID     int64 // 0: to be created
	Actions     []Action
	monitorsRaw map[int64]kuma.Object
}

// HeartbeatInterval is the Kuma push monitor's interval for a check: the
// check's interval plus the longest a run can take (every attempt timing
// out, with the pauses between), plus 10 seconds of slack, and at least
// Kuma's minimum of 20 seconds.
func HeartbeatInterval(c config.Check, retryPause time.Duration) int {
	retries := 0
	if c.Retries != nil {
		retries = *c.Retries
	}
	run := time.Duration(c.Timeout)*time.Duration(retries+1) + retryPause*time.Duration(retries)
	secs := int(math.Ceil((time.Duration(c.Interval) + run + 10*time.Second).Seconds()))
	return max(secs, 20)
}

// MakePlan works out what to do, from the monitors the server has.
func MakePlan(c *config.Config, monitors []kuma.Object, retryPause time.Duration, newToken func() string) (*Plan, error) {
	group := c.Targets.Kuma.Group
	if group == "" {
		group = config.DefaultGroup
	}
	p := &Plan{Group: group, monitorsRaw: map[int64]kuma.Object{}}
	byToken := map[string]kuma.Object{}
	byName := map[string]kuma.Object{} // push monitors in the group
	for _, m := range monitors {
		p.monitorsRaw[m.ID()] = m
		if m.Str("type") == "group" && m.Str("name") == group && kuma.Int(m["parent"]) == 0 {
			p.GroupID = m.ID()
		}
	}
	for _, m := range monitors {
		if m.Str("type") != "push" {
			continue
		}
		if t := m.Str("pushToken"); t != "" {
			byToken[t] = m
		}
		if p.GroupID != 0 && kuma.Int(m["parent"]) == p.GroupID {
			byName[m.Str("name")] = m
		}
	}
	used := map[string]string{}
	for _, ch := range c.Checks {
		if ch.Kuma == nil {
			continue
		}
		a := Action{Check: ch.Name, Name: ch.Name, Token: ch.Kuma.PushToken, Var: ch.Kuma.TokenVar(),
			Interval: HeartbeatInterval(ch, retryPause)}
		if other, dup := used[a.Token]; dup && a.Token != "" {
			return nil, fmt.Errorf("checks %q and %q have the same push token; each needs its own monitor", other, ch.Name)
		}
		var have kuma.Object
		if a.Token != "" {
			have = byToken[a.Token]
		}
		if have == nil {
			have = byName[ch.Name]
			if have != nil && a.Token != "" && have.Str("pushToken") != a.Token {
				a.Changes = append(a.Changes, "push token (set to the file's)")
			}
		}
		switch {
		case have == nil:
			a.Op = "create"
			if a.Token == "" {
				a.Token, a.NewToken = newToken(), true
			}
		default:
			a.ID, a.Name = have.ID(), have.Str("name")
			if a.Token == "" {
				a.Token = have.Str("pushToken")
			}
			if n := int(kuma.Int(have["interval"])); n != a.Interval {
				a.Changes = append(a.Changes, fmt.Sprintf("heartbeat interval %ds -> %ds", n, a.Interval))
			}
			a.Op = "ok"
			if len(a.Changes) > 0 {
				a.Op = "update"
			}
		}
		used[a.Token] = ch.Name
		p.Actions = append(p.Actions, a)
	}
	return p, nil
}

// NewToken makes a push token: 26 random letters and digits (130 bits).
func NewToken() string { return rand.Text() }

// derived are monitor fields the server makes itself; they are not sent back.
var derived = map[string]bool{"id": true, "path": true, "pathName": true, "childrenIDs": true, "maintenance": true,
	"tags": true, "includeSensitiveData": true, "forceInactive": true, "screenshot": true}

// Apply carries out a plan. done is called after each monitor that was
// created or changed (so tokens can be saved even if a later one fails).
func Apply(ctx context.Context, c *kuma.Client, p *Plan, done func(Action)) error {
	if p.GroupID == 0 && needsGroup(p) {
		r, err := c.Do(ctx, "add", map[string]any{"type": "group", "name": p.Group, "accepted_statuscodes": []string{"200-299"},
			"notificationIDList": map[string]any{}, "conditions": []any{}, "interval": 60, "retryInterval": 60, "active": true})
		if err != nil {
			return fmt.Errorf("creating the group %q: %w", p.Group, err)
		}
		p.GroupID = kuma.Int(r["monitorID"])
	}
	for _, a := range p.Actions {
		switch a.Op {
		case "create":
			_, err := c.Do(ctx, "add", map[string]any{"type": "push", "name": a.Name, "pushToken": a.Token, "parent": p.GroupID,
				"interval": a.Interval, "retryInterval": a.Interval, "maxretries": 0, "accepted_statuscodes": []string{"200-299"},
				"notificationIDList": map[string]any{}, "conditions": []any{}, "active": true})
			if err != nil {
				return fmt.Errorf("creating the monitor %q: %w", a.Name, err)
			}
		case "update":
			// Everything the server has, so nothing sync does not manage
			// is lost (the server sets every field it is sent).
			payload := map[string]any{}
			raw, _ := json.Marshal(p.monitorsRaw[a.ID])
			json.Unmarshal(raw, &payload)
			for k := range derived {
				delete(payload, k)
			}
			codes := []string{}
			if l, ok := payload["accepted_statuscodes"].([]any); ok {
				for _, s := range l {
					codes = append(codes, fmt.Sprint(s))
				}
			}
			payload["accepted_statuscodes"] = codes
			if payload["conditions"] == nil {
				payload["conditions"] = []any{}
			}
			if payload["notificationIDList"] == nil {
				payload["notificationIDList"] = map[string]any{}
			}
			payload["id"] = a.ID
			payload["pushToken"] = a.Token
			payload["interval"] = a.Interval
			payload["retryInterval"] = a.Interval
			if _, err := c.Do(ctx, "editMonitor", payload); err != nil {
				return fmt.Errorf("changing the monitor %q: %w", a.Name, err)
			}
		default:
			continue
		}
		done(a)
	}
	return nil
}

func needsGroup(p *Plan) bool {
	for _, a := range p.Actions {
		if a.Op == "create" {
			return true
		}
	}
	return false
}

// Describe is a plan's lines for people. saves says whether an action's
// token is saved this time. Tokens are never shown.
func Describe(p *Plan, saves func(Action) bool) []string {
	var out []string
	if p.GroupID == 0 && needsGroup(p) {
		out = append(out, fmt.Sprintf("+ group %q", p.Group))
	}
	for _, a := range p.Actions {
		where := ""
		if saves(a) {
			where = ", token saved as " + a.Var
		}
		switch a.Op {
		case "create":
			token := "the file's token"
			if a.NewToken {
				token = "a new token"
			}
			out = append(out, fmt.Sprintf("+ push monitor %q (heartbeat every %ds, %s%s)", a.Name, a.Interval, token, where))
		case "update":
			out = append(out, fmt.Sprintf("~ push monitor %q: %s%s", a.Name, strings.Join(a.Changes, ", "), where))
		default:
			out = append(out, fmt.Sprintf("= push monitor %q%s", a.Name, where))
		}
	}
	return out
}
