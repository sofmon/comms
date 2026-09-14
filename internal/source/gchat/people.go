package gchat

import (
	"context"
	"fmt"
	"strings"
	"time"

	people "google.golang.org/api/people/v1"

	"comms/internal/retry"
)

const (
	peopleBatchSize          = 200
	resolvedNameRefresh      = 30 * 24 * time.Hour
	unresolvedNameRetry      = 24 * time.Hour
	peopleProfileSource      = "READ_SOURCE_TYPE_PROFILE"
	peopleProfilePersonField = "names"
)

// peopleAPI is the narrow People API seam needed for optional Chat sender
// enrichment. LookupNames returns an entry for every requested users/{id}; an
// empty value means Google did not expose a usable profile name.
type peopleAPI interface {
	probe(context.Context) error
	lookupNames(context.Context, []string) (map[string]string, error)
}

type realPeopleAPI struct {
	svc *people.Service
}

func (p *realPeopleAPI) probe(ctx context.Context) error {
	_, err := p.svc.People.Get("people/me").
		PersonFields(peopleProfilePersonField).
		Sources(peopleProfileSource).
		Context(ctx).
		Do()
	return err
}

func (p *realPeopleAPI) lookupNames(ctx context.Context, userIDs []string) (map[string]string, error) {
	resources := make([]string, len(userIDs))
	out := make(map[string]string, len(userIDs))
	for i, id := range userIDs {
		resources[i] = "people/" + strings.TrimPrefix(id, "users/")
		out[id] = ""
	}
	resp, err := p.svc.People.GetBatchGet().
		ResourceNames(resources...).
		PersonFields(peopleProfilePersonField).
		Sources(peopleProfileSource).
		Context(ctx).
		Do()
	if err != nil {
		return nil, err
	}
	for i, r := range resp.Responses {
		if r == nil || (r.Status != nil && r.Status.Code != 0) || r.Person == nil {
			continue
		}
		id := strings.TrimPrefix(r.RequestedResourceName, "people/")
		if id == "" && i < len(userIDs) {
			id = strings.TrimPrefix(userIDs[i], "users/")
		}
		key := "users/" + id
		if _, requested := out[key]; requested {
			out[key] = preferredPersonName(r.Person.Names)
		}
	}
	return out, nil
}

// preferredPersonName is deterministic even if Google changes the order of
// the returned name records: primary wins, then source-primary, then the
// lexical first non-empty display name.
func preferredPersonName(names []*people.Name) string {
	best, bestRank := "", 3
	for _, n := range names {
		if n == nil {
			continue
		}
		name := strings.TrimSpace(n.DisplayName)
		if name == "" {
			continue
		}
		rank := 2
		if n.Metadata != nil && n.Metadata.Primary {
			rank = 0
		} else if n.Metadata != nil && n.Metadata.SourcePrimary {
			rank = 1
		}
		if rank < bestRank || (rank == bestRank && (best == "" || name < best)) {
			best, bestRank = name, rank
		}
	}
	return best
}

// CheckNameResolution is a non-mutating People API probe used by doctor.
// Chat archiving itself treats enrichment failures as advisory.
func (c *Connector) CheckNameResolution(ctx context.Context) error {
	if !c.acct.ResolveChatNames {
		return nil
	}
	if c.people == nil {
		return fmt.Errorf("gchat: People API client is not configured")
	}
	if err := c.people.probe(ctx); err != nil {
		return fmt.Errorf("gchat: resolve chat names: %w", err)
	}
	return nil
}

// refreshSenderNames applies local overrides and enriches every due sender id
// already present in the canonical message store. Provider failures are
// warnings and never stop message archival; state failures remain fatal.
func (c *Connector) refreshSenderNames(ctx context.Context) error {
	if !c.acct.ResolveChatNames {
		return nil
	}
	if c.people == nil {
		c.log.Warn("gchat: display-name enrichment is enabled but no People API client is configured")
		return nil
	}
	now := c.now()
	ids, err := c.db.ChatPeopleDue(c.src, now.Add(-resolvedNameRefresh), now.Add(-unresolvedNameRetry))
	if err != nil {
		return err
	}
	resolved := 0
	for start := 0; start < len(ids); start += peopleBatchSize {
		end := min(start+peopleBatchSize, len(ids))
		batch := ids[start:end]
		var names map[string]string
		err := retry.Do(ctx, c.retryOpts, func() error {
			got, err := c.people.lookupNames(ctx, batch)
			if err != nil {
				return err
			}
			names = got
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("gchat: People API name lookup failed; opaque user ids remain in the archive", "count", len(batch), "err", err)
			continue
		}
		for _, id := range batch {
			name := names[id]
			if _, err := c.db.SetChatPersonFromPeople(c.src, id, name); err != nil {
				return err
			}
			if name != "" {
				resolved++
			}
		}
	}
	if resolved > 0 {
		c.log.Info("gchat: refreshed sender display names", "resolved", resolved, "checked", len(ids))
	}
	return nil
}
