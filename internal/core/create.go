package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/agentfeedback/agentfeedback/v4/internal/store"
	"github.com/agentfeedback/agentfeedback/v4/pkg/canonjson"
	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

// CreateResult is the outcome of a create: the stored record (new or
// existing), the decoder's warnings (on every outcome, replays included) and
// whether a row was inserted (201) or an existing one returned (200).
type CreateResult struct {
	Record   Record
	Warnings []schema.Detail
	Created  bool
}

// MarshalJSON writes the CreateResponse body {submission, warnings}.
func (c CreateResult) MarshalJSON() ([]byte, error) {
	warnings := c.Warnings
	if warnings == nil {
		warnings = []schema.Detail{}
	}
	return Marshal(struct {
		Submission Record          `json:"submission"`
		Warnings   []schema.Detail `json:"warnings"`
	}{c.Record, warnings})
}

// Create runs the write path on body and applies content identity in one
// write transaction. A keyed body whose (kind, key) exists returns that row
// when the content hash matches and is 409 replay_mismatch otherwise. A
// keyless body returns the newest unprocessed row with the same hash created
// within DedupeWindow. Anything else is inserted with a new UUIDv7 and
// created_at = now. A body Decode rejects is a Problem with its status.
func (s *Service) Create(ctx context.Context, body []byte) (CreateResult, error) {
	env, warnings, err := envelope.Decode(body)
	if err != nil {
		var r *envelope.Rejection
		if errors.As(err, &r) {
			return CreateResult{}, &Problem{Status: r.Status, Code: r.Code, Message: r.Message}
		}
		return CreateResult{}, err
	}
	sub, err := submissionOf(env)
	if err != nil {
		return CreateResult{}, err
	}
	sub.ContentHash = env.ContentHash()

	now := s.nowMicros()
	result := CreateResult{Warnings: warnings}
	err = s.write(ctx, func(q store.Querier) error {
		var existing store.Submission
		var err error
		if sub.Key != "" {
			existing, err = store.GetByKey(ctx, q, sub.Kind, sub.Key)
			if err == nil && existing.ContentHash != sub.ContentHash {
				msg := fmt.Sprintf("key %q already names submission %d with different content", sub.Key, existing.ID)
				return &Problem{Status: 409, Code: CodeReplayMismatch,
					Message: fmt.Sprintf("key %q was already used with different content", sub.Key),
					Details: []Detail{{Code: "key_reused", Pointer: "/key", Message: msg, ExistingID: existing.ID}}}
			}
		} else {
			existing, err = store.GetRecentByHash(ctx, q, sub.ContentHash, now-DedupeWindow.Microseconds())
		}
		switch {
		case err == nil:
			result.Record = Record{existing}
			return nil
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if sub.UID, err = s.newUID(); err != nil {
			return err
		}
		sub.CreatedAt = now
		id, err := store.Insert(ctx, q, sub)
		if store.IsUniqueViolation(err) {
			// The lookups above ran in the same BEGIN IMMEDIATE
			// transaction, so only a uid collision gets here.
			return fmt.Errorf("insert after identity lookups: %w", err)
		}
		if err != nil {
			return err
		}
		sub.ID = id
		result.Record = Record{sub}
		result.Created = true
		return nil
	})
	if err != nil {
		return CreateResult{}, err
	}
	return result, nil
}

// submissionOf maps a decoded envelope onto the store's row: payload and
// context as canonical JSON bytes, occurred_at as unix microseconds. Server
// fields are left for the caller.
func submissionOf(env *envelope.Envelope) (store.Submission, error) {
	tree := env.Tree()
	sub := store.Submission{
		Kind:          env.Kind,
		SchemaVersion: int64(env.SchemaVersion),
		Key:           env.Key,
		Summary:       env.Summary,
		Machine:       env.Machine,
		Model:         env.Model,
		Harness:       env.Harness,
		Project:       env.Project,
		Payload:       canonjson.Marshal(env.Payload),
	}
	if c, ok := tree["context"]; ok {
		sub.Context = canonjson.Marshal(c)
	}
	if env.OccurredAt != "" {
		micros, ok := parseStoredTime(env.OccurredAt)
		if !ok {
			return store.Submission{}, fmt.Errorf("decoder produced an unparseable occurred_at %q", env.OccurredAt)
		}
		sub.OccurredAt = &micros
	}
	return sub, nil
}
