package memtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// Person profiles (V1): the curator rewrites one with update_profile, only
// with a reason; a reply reads one with read_profile when it needs to know
// who someone is.

const maxProfileReason = 200

func (t *Turn) updateProfileTool() llm.Tool {
	return llm.Tool{
		Name: "update_profile",
		Description: "Rewrite a participant's profile: a prose picture of who they are. Call it only with a real reason (see the system prompt), never just to reword. " +
			"The text replaces the old profile, so keep everything from it that is still true. Returns updated, or an error to fix.",
		Schema: map[string]any{"type": "object", "required": []string{"subject", "profile", "reason"}, "properties": map[string]any{
			"subject": map[string]any{"type": "string", "description": "The participant's handle, like P2."},
			"profile": map[string]any{"type": "string", "description": fmt.Sprintf("The whole new profile, in English prose, at most %d words, naming people by name, never by handle.", store.MaxProfileWords)},
			"reason":  map[string]any{"type": "string", "description": "What in the new messages makes the rewrite worth it, in one short sentence."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct{ Subject, Profile, Reason string }
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			out, err := t.updateProfile(ctx, in.Subject, in.Profile, in.Reason)
			if err != nil {
				log.Printf("update_profile rejected: %v", err)
				return errText(err)
			}
			return out
		},
	}
}

func (t *Turn) updateProfile(ctx context.Context, subject, text, reason string) (string, error) {
	h := strings.ToUpper(strings.TrimSpace(subject))
	reason = strings.Join(strings.Fields(reason), " ")
	text = strings.TrimSpace(text)
	key := t.Handles[h]
	switch {
	case !handleRe.MatchString(h) || key == "":
		return "", errors.New("subject must be the handle of a participant in <people>, like P2")
	case reason == "":
		return "", errors.New("reason is empty: only rewrite a profile when the new messages give a reason")
	case utf8.RuneCountInString(reason) > maxProfileReason:
		return "", fmt.Errorf("reason is too long (max %d characters)", maxProfileReason)
	case text == "":
		return "", errors.New("profile is empty")
	case len(strings.Fields(text)) > store.MaxProfileWords:
		return "", fmt.Errorf("profile is too long (max %d words); tighten it", store.MaxProfileWords)
	case router.MergesIdentities(text):
		return "", errors.New("not saved: never say two people are the same person, interchangeable or alts")
	}
	text = router.StripHandles(text, t.Names) // K6: handles are per prompt
	if err := t.DB.SetProfile(ctx, key, text, reason); err != nil {
		log.Printf("update profile: %v", err)
		return "", errors.New("storage failed")
	}
	t.saved.Add(1)
	log.Printf("profile of %s (%s) rewritten, %d words: %s", h, key, len(strings.Fields(text)), reason)
	return "updated", nil
}

func (t *Turn) readProfileTool() llm.Tool {
	return llm.Tool{
		Name: "read_profile",
		Description: "Read a person's profile: a prose picture of who they are, built up from everything they have said over time (character, background, relationships, interests, views, how they talk and how they treat you). " +
			"Read it when your reply depends on who someone is: advice or opinions for them, personal questions, banter, talking about them, or matching how to talk to them. Not needed for plain lookups or small talk. For specific details use search_facts.",
		Schema: map[string]any{"type": "object", "required": []string{"person"}, "properties": map[string]any{
			"person": map[string]any{"type": "string", "description": "A handle like P2, or a name for someone from another chat."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct{ Person string }
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			person := strings.TrimSpace(in.Person)
			if person == "" {
				return "error: person is empty"
			}
			keys, err := t.keysFor(ctx, person)
			if err != nil {
				return "error: lookup failed"
			}
			if len(keys) == 0 {
				return "no one known as " + person
			}
			var b strings.Builder
			for _, k := range keys {
				p, err := t.DB.GetProfile(ctx, k)
				if err != nil {
					return "error: storage failed"
				}
				if p.Text == "" {
					continue
				}
				fmt.Fprintf(&b, "profile of %s (updated %s):\n%s\n\n", t.label(ctx, k), p.Updated.Format("2006-01-02"), p.Text)
			}
			log.Printf("read_profile %q -> %d keys", person, len(keys))
			if b.Len() == 0 {
				return "no profile of " + person + " yet; use search_facts"
			}
			return strings.TrimSpace(b.String())
		},
	}
}
