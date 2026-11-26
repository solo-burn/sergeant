package slackapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"sergeant/internal/i18n"
	"sergeant/internal/legacy"
	"sergeant/internal/parser"
	"sergeant/internal/shadow"
)

const (
	testSecret = "test-signing-secret"
	botID      = "USERGEANT"
)

type post struct {
	channel, target, text string
	ephemeral             bool
}

type reaction struct{ channel, ts, name string }

type spy struct {
	mu          sync.Mutex
	calls       []post
	views       []slack.ModalViewRequest
	triggers    []string
	reactions   []reaction
	reactionErr error
	postErr     error
	done        chan struct{}
}

func newSpy() *spy { return &spy{done: make(chan struct{}, 8)} }

func (s *spy) PostInThread(_ context.Context, ch, ts, text string) error {
	return s.add(post{ch, ts, text, false})
}
func (s *spy) PostEphemeral(_ context.Context, ch, user, text string) error {
	return s.add(post{ch, user, text, true})
}
func (s *spy) PostEphemeralBlocks(_ context.Context, ch, user, fallback string, _ []slack.Block) error {
	return s.add(post{ch, user, fallback, true})
}
func (s *spy) OpenView(_ context.Context, triggerID string, view slack.ModalViewRequest) error {
	s.mu.Lock()
	s.triggers = append(s.triggers, triggerID)
	s.views = append(s.views, view)
	s.mu.Unlock()
	s.done <- struct{}{}
	return nil
}
func (s *spy) AddReaction(_ context.Context, channel, ts, name string) error {
	s.mu.Lock()
	s.reactions = append(s.reactions, reaction{channel, ts, name})
	s.mu.Unlock()
	return s.reactionErr
}
func (s *spy) add(p post) error {
	s.mu.Lock()
	s.calls = append(s.calls, p)
	s.mu.Unlock()
	s.done <- struct{}{}
	return s.postErr
}
func (s *spy) wait(t *testing.T) post {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for responder")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

// dispatch delivers synchronously so tests can inspect reactions and replies.
func dispatch(t *testing.T, h *Handler, event slackevents.EventsAPIEvent) {
	t.Helper()
	notice, err := h.processEvent(t.Context(), event)
	if err != nil {
		t.Fatalf("process event: %v", err)
	}
	if notice != nil {
		h.deliver(notice)
	}
}

func newHandler(t *testing.T) (*Handler, *spy, *legacy.Engine) {
	t.Helper()
	return newHandlerAt(t, filepath.Join(t.TempDir(), "legacy.db"))
}

func newHandlerAt(t *testing.T, primaryPath string) (*Handler, *spy, *legacy.Engine) {
	t.Helper()
	tr := i18n.New("en")
	ledgerPath := filepath.Join(filepath.Dir(primaryPath), "ledger.db")
	trial, err := shadow.Open(t.Context(), primaryPath, ledgerPath, "PLN", tr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trial.Legacy.Close() })
	sp := newSpy()
	h := &Handler{
		SigningSecret: testSecret,
		BotUserID:     botID,
		Engine:        trial.Legacy,
		Responder:     sp,
		Translator:    tr,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	t.Cleanup(h.Wait)
	return h, sp, trial.Legacy
}

func assertPairStatus(t *testing.T, accounts *legacy.Engine, target, want string) {
	t.Helper()
	cmd := parser.Command{Kind: parser.KindStatusFor, Target: target}
	reply, err := accounts.ApplyOnce(t.Context(), "", "UAAA", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != want {
		t.Fatalf("status for %s = %q, want %q", target, reply.Text, want)
	}
}

func sign(t *testing.T, body []byte, ts int64) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = fmt.Fprintf(mac, "v0:%d:", ts)
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func appMention(text string) []byte {
	return []byte(fmt.Sprintf(`{"type":"event_callback","event":{"type":"app_mention","user":"UAAA","text":%q,"channel":"C1","ts":"123.456"}}`, text))
}

func parseCallbackEvent(t *testing.T, body []byte) slackevents.EventsAPIEvent {
	t.Helper()
	event, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestVerification(t *testing.T) {
	h, _, _ := newHandler(t)
	body := []byte(`{"type":"event_callback"}`)

	t.Run("bad signature", func(t *testing.T) {
		req := sign(t, body, time.Now().Unix())
		req.Header.Set("X-Slack-Signature", "v0=deadbeef")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Code)
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		req := sign(t, body, time.Now().Add(-10*time.Minute).Unix())
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Code)
		}
	})
}

func TestURLVerification(t *testing.T) {
	h, _, _ := newHandler(t)
	body := []byte(`{"type":"url_verification","challenge":"abc123"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, body, time.Now().Unix()))
	if rec.Code != http.StatusOK || rec.Body.String() != "abc123" {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestAppMentionDispatch(t *testing.T) {
	usageReply := ":cop: " + i18n.New("en").T(i18n.HandlerUsage)
	tests := []struct {
		name      string
		text      string
		ephemeral bool
		want      string
		// where the reply must go: thread ts ("123.456") for non-ephemeral,
		// user ID ("UAAA") for ephemeral
		target string
	}{
		{name: "status all", text: "<@USERGEANT> status", ephemeral: false, want: ":cop: Nobody owes you and you owe nothing.", target: "123.456"},
		{
			name: "empty pair status", text: "<@USERGEANT> <@UBBB> status", target: "123.456",
			want: ":cop: <@UBBB> owes you nothing.",
		},
		{
			name: "empty payment methods", text: "<@USERGEANT> pay me", target: "123.456",
			want: ":cop: You haven't added any payment methods yet. Try " +
				"`@sergeant pay set bank PL61 ...` or `@sergeant pay set blik 555 555 555`.",
		},
		{name: "unrecognized", text: "<@USERGEANT> ¿qué?", ephemeral: true, want: usageReply, target: "UAAA"},
		{name: "bare mention", text: "<@USERGEANT>", ephemeral: true, want: usageReply, target: "UAAA"},
		{name: "bad amount", text: "<@USERGEANT> <@UBBB> +abc", ephemeral: true, want: usageReply, target: "UAAA"},
		{name: "bad currency", text: "<@USERGEANT> <@UBBB> +20 PLNS", ephemeral: true, want: usageReply, target: "UAAA"},
		{name: "unknown pay subcommand", text: "<@USERGEANT> pay nonsense", ephemeral: true, want: usageReply, target: "UAAA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, sp, _ := newHandler(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, sign(t, appMention(tc.text), time.Now().Unix()))
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d", rec.Code)
			}
			p := sp.wait(t)
			if p.ephemeral != tc.ephemeral {
				t.Errorf("ephemeral=%v, want %v", p.ephemeral, tc.ephemeral)
			}
			if p.target != tc.target {
				t.Errorf("target=%q, want %q", p.target, tc.target)
			}
			if p.text != tc.want {
				t.Errorf("text:\n  got:  %q\n  want: %q", p.text, tc.want)
			}
		})
	}
}

func TestAppMentionSuccessfulQueryAddsSuccessReactions(t *testing.T) {
	h, sp, _ := newHandler(t)

	dispatch(t, h, parseCallbackEvent(t, appMention("<@USERGEANT> status")))

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.reactions) != 2 {
		t.Fatalf("reactions = %v, want 2", sp.reactions)
	}
	if got := sp.reactions[0]; got.channel != "C1" || got.ts != "123.456" || got.name != mentionEmoji {
		t.Errorf("acknowledgement reaction = %+v", got)
	}
	if got := sp.reactions[1]; got.channel != "C1" || got.ts != "123.456" || got.name != successEmoji {
		t.Errorf("success reaction = %+v", got)
	}
}

func TestSuccessfulHandlerCommandsAddSuccessReactions(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "help", command: "<@USERGEANT> help"},
		{name: "hello", command: "<@USERGEANT> hello"},
		{name: "payment form", command: "<@USERGEANT> pay set"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, sp, _ := newHandler(t)

			dispatch(t, h, parseCallbackEvent(t, appMention(tc.command)))

			sp.mu.Lock()
			defer sp.mu.Unlock()
			if len(sp.reactions) != 2 || sp.reactions[0].name != mentionEmoji || sp.reactions[1].name != successEmoji {
				t.Fatalf("reactions = %v, want %q followed by %q", sp.reactions, mentionEmoji, successEmoji)
			}
		})
	}
}

func TestFailedResponseDoesNotAddSuccessReaction(t *testing.T) {
	h, sp, _ := newHandler(t)
	sp.postErr = errors.New("post unavailable")

	dispatch(t, h, parseCallbackEvent(t, appMention("<@USERGEANT> status")))

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.reactions) != 1 || sp.reactions[0].name != mentionEmoji {
		t.Fatalf("reactions = %v, want only %q acknowledgement", sp.reactions, mentionEmoji)
	}
}

func TestDirectMessageDebtMutationUsesReactionOnly(t *testing.T) {
	h, sp, l := newHandler(t)

	dispatch(t, h, parseCallbackEvent(t, directMessage("<@UBBB> +20 PLN")))

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.reactions) != 2 || sp.reactions[0].name != mentionEmoji || sp.reactions[1].name != successEmoji {
		t.Fatalf("reactions = %v, want %q followed by %q", sp.reactions, mentionEmoji, successEmoji)
	}
	if len(sp.calls) != 0 {
		t.Fatalf("DM debt mutation should not post a message, got %v", sp.calls)
	}
	r, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: "UBBB"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "<@UBBB> owes you 20.00 PLN."; r.Text != want {
		t.Fatalf("account status: got %q, want %q", r.Text, want)
	}
}

func TestAppMentionDebtMutationsUseReactionOnly(t *testing.T) {
	tests := []struct {
		name         string
		initialMinor int64
		command      string
		wantStatus   string
	}{
		{name: "add", command: "<@USERGEANT> <@UBBB> +20 PLN", wantStatus: "<@UBBB> owes you 20.00 PLN."},
		{name: "subtract", initialMinor: 2000, command: "<@USERGEANT> <@UBBB> -5 PLN", wantStatus: "<@UBBB> owes you 15.00 PLN."},
		{name: "reset", initialMinor: 2000, command: "<@USERGEANT> <@UBBB> reset", wantStatus: "<@UBBB> owes you nothing."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, sp, l := newHandler(t)
			if tc.initialMinor > 0 {
				if _, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{
					Kind: parser.KindAdd, Target: "UBBB", Sign: 1, Minor: tc.initialMinor, Currency: "PLN",
				}); err != nil {
					t.Fatal(err)
				}
			}

			dispatch(t, h, parseCallbackEvent(t, appMention(tc.command)))

			sp.mu.Lock()
			defer sp.mu.Unlock()
			if len(sp.reactions) != 2 || sp.reactions[0].name != mentionEmoji || sp.reactions[1].name != successEmoji {
				t.Fatalf("reactions = %v, want %q followed by %q", sp.reactions, mentionEmoji, successEmoji)
			}
			if len(sp.calls) != 0 {
				t.Fatalf("debt mutation should not post a message, got %v", sp.calls)
			}
			r, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: "UBBB"})
			if err != nil {
				t.Fatal(err)
			}
			if r.Text != tc.wantStatus {
				t.Fatalf("account status: got %q, want %q", r.Text, tc.wantStatus)
			}
		})
	}
}

func TestAppMentionDebtMutationDoesNotReplyWhenReactionFails(t *testing.T) {
	h, sp, l := newHandler(t)
	sp.reactionErr = errors.New("reaction unavailable")

	dispatch(t, h, parseCallbackEvent(t, appMention("<@USERGEANT> <@UBBB> +20 PLN")))

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.calls) != 0 {
		t.Fatalf("debt mutation should remain silent when the reaction fails, got %v", sp.calls)
	}
	r, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: "UBBB"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "<@UBBB> owes you 20.00 PLN."; r.Text != want {
		t.Fatalf("account status: got %q, want %q", r.Text, want)
	}
}

func TestMultilineDebtMessageUpdatesEveryPerson(t *testing.T) {
	for _, source := range []struct {
		name    string
		message func(string) []byte
		prefix  string
	}{
		{name: "channel mention", message: appMention, prefix: "<@USERGEANT>\n"},
		{name: "direct message", message: directMessage},
	} {
		t.Run(source.name, func(t *testing.T) {
			// Arrange
			h, sp, l := newHandler(t)
			event := parseCallbackEvent(t, source.message(source.prefix+"<@UBBB> +39\n<@UCCC> +41\n<@UDDD> +43"))
			wantStatuses := map[string]string{
				"UBBB": "<@UBBB> owes you 39.00 PLN.",
				"UCCC": "<@UCCC> owes you 41.00 PLN.",
				"UDDD": "<@UDDD> owes you 43.00 PLN.",
			}

			// Act
			dispatch(t, h, event)

			// Assert
			for target, want := range wantStatuses {
				r, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: target})
				if err != nil {
					t.Fatal(err)
				}
				if r.Text != want {
					t.Errorf("account status: got %q, want %q", r.Text, want)
				}
			}
			if len(sp.calls) != 0 {
				t.Errorf("debt message should not post a reply, got %v", sp.calls)
			}
			if len(sp.reactions) != 2 || sp.reactions[0].name != mentionEmoji || sp.reactions[1].name != successEmoji {
				t.Errorf("reactions = %v, want %q followed by %q", sp.reactions, mentionEmoji, successEmoji)
			}
		})
	}
}

func TestMultilineDebtMessageListsFailuresAndContinues(t *testing.T) {
	for _, tc := range []struct {
		name, locale, prefix, wantText, wantTarget, wantChannel string
		message                                                 func(string) []byte
	}{
		{
			name: "channel", locale: "en", prefix: "<@USERGEANT>\n", message: appMention,
			wantTarget: "123.456", wantChannel: "C1",
			wantText: ":cop: These updates failed; all other updates were saved:\n- <@UAAA> (update 1)\n- <@UAAA> (update 3)",
		},
		{
			name: "direct message", locale: "en", message: directMessage, wantTarget: "123.456", wantChannel: "D1",
			wantText: ":cop: These updates failed; all other updates were saved:\n- <@UAAA> (update 1)\n- <@UAAA> (update 3)",
		},
		{
			name: "Polish channel", locale: "pl", prefix: "<@USERGEANT>\n", message: appMention,
			wantTarget: "123.456", wantChannel: "C1",
			wantText: ":cop: Te zmiany się nie powiodły; wszystkie pozostałe zostały zapisane:\n- <@UAAA> (zmiana 1)\n- <@UAAA> (zmiana 3)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			h, sp, l := newHandler(t)
			h.Translator = i18n.New(tc.locale)
			event := parseCallbackEvent(t, tc.message(tc.prefix+"<@UAAA> +1\n<@UBBB> +39\n<@UAAA> +2\n<@UCCC> +41"))
			wantStatuses := map[string]string{
				"UBBB": "<@UBBB> owes you 39.00 PLN.",
				"UCCC": "<@UCCC> owes you 41.00 PLN.",
			}

			// Act
			dispatch(t, h, event)

			// Assert
			for target, want := range wantStatuses {
				r, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: target})
				if err != nil {
					t.Fatal(err)
				}
				if r.Text != want {
					t.Errorf("account status: got %q, want %q", r.Text, want)
				}
			}
			if len(sp.calls) != 1 {
				t.Fatalf("got %v replies, want one failure list", sp.calls)
			}
			if got := sp.calls[0]; got.text != tc.wantText || got.ephemeral || got.target != tc.wantTarget || got.channel != tc.wantChannel {
				t.Errorf("unexpected failure reply: %+v", got)
			}
			if len(sp.reactions) != 1 || sp.reactions[0].name != failureEmoji {
				t.Errorf("reactions = %v, want only %q", sp.reactions, failureEmoji)
			}
		})
	}
}

func TestMultilineDebtMessageReportsInvalidEntry(t *testing.T) {
	for _, tc := range []struct{ name, lastLine, wantStatus string }{
		{name: "malformed amount", lastLine: "<@UCCC> +oops", wantStatus: "<@UBBB> owes you nothing."},
		{name: "self target after successful adjustment", lastLine: "<@UAAA> +41", wantStatus: "<@UBBB> owes you 39.00 PLN."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			h, sp, l := newHandler(t)
			event := parseCallbackEvent(t, appMention("<@USERGEANT>\n<@UBBB> +39\n"+tc.lastLine))

			// Act
			dispatch(t, h, event)

			// Assert
			r, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: "UBBB"})
			if err != nil {
				t.Fatal(err)
			}
			if r.Text != tc.wantStatus {
				t.Fatalf("account status: got %q, want %q", r.Text, tc.wantStatus)
			}
			var foundError bool
			for _, reaction := range sp.reactions {
				if reaction.name == successEmoji {
					t.Error("invalid batch must not get a success reaction")
				}
				foundError = foundError || reaction.name == failureEmoji
			}
			if !foundError {
				t.Error("invalid batch must get an error reaction")
			}
		})
	}
}

func TestFailedDebtMutationUsesErrorReactionOnly(t *testing.T) {
	tests := []struct {
		name  string
		event []byte
	}{
		{name: "channel", event: appMention("<@USERGEANT> <@UAAA> +20 PLN")},
		{name: "direct message", event: directMessage("<@UAAA> +20 PLN")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, sp, _ := newHandler(t)

			dispatch(t, h, parseCallbackEvent(t, tc.event))

			sp.mu.Lock()
			defer sp.mu.Unlock()
			if len(sp.reactions) != 1 || sp.reactions[0].name != failureEmoji {
				t.Fatalf("reactions = %v, want one %q reaction", sp.reactions, failureEmoji)
			}
			if len(sp.calls) != 0 {
				t.Fatalf("failed debt mutation should not post a message, got %v", sp.calls)
			}
		})
	}
}

func signForm(t *testing.T, body string, ts int64) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = fmt.Fprintf(mac, "v0:%d:", ts)
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestPaySetFormPostsOpenerButton(t *testing.T) {
	h, sp, _ := newHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, appMention("<@USERGEANT> pay set"), time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	p := sp.wait(t)
	if !p.ephemeral {
		t.Errorf("opener should be ephemeral")
	}
	if want := "Add a payment method privately"; p.text != want {
		t.Errorf("text: got %q, want %q", p.text, want)
	}
}

func TestBlockActionOpensModal(t *testing.T) {
	h, sp, _ := newHandler(t)

	payload := slack.InteractionCallback{
		Type:      slack.InteractionTypeBlockActions,
		User:      slack.User{ID: "UAAA"},
		TriggerID: "trig.123",
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionOpenPayForm},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := url.Values{"payload": []string{string(raw)}}.Encode()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signForm(t, body, time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	select {
	case <-sp.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OpenView")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.triggers) != 1 || sp.triggers[0] != "trig.123" {
		t.Fatalf("triggers = %v", sp.triggers)
	}
	if sp.views[0].CallbackID != callbackPayForm {
		t.Fatalf("modal callback = %q", sp.views[0].CallbackID)
	}
}

func TestViewSubmissionStoresAndConfirms(t *testing.T) {
	h, _, l := newHandler(t)

	// Build a minimal view_submission payload by hand because slack.View's
	// State.Values shape isn't a public construction-friendly API.
	payloadJSON := fmt.Sprintf(`{
		"type":"view_submission",
		"user":{"id":"UAAA"},
		"view":{
			"callback_id":%q,
			"state":{"values":{
				%q:{%q:{"selected_option":{"value":"bank"}}},
				%q:{%q:{"value":"PL61 1090 0000"}}
			}}
		}
	}`, callbackPayForm, blockMethod, actionMethod, blockValue, actionValue)

	body := url.Values{"payload": []string{payloadJSON}}.Encode()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signForm(t, body, time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		ResponseAction string `json:"response_action"`
		View           struct {
			Blocks []struct {
				Type string `json:"type"`
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"blocks"`
		} `json:"view"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %s", rec.Body.String())
	}
	if resp.ResponseAction != "update" {
		t.Fatalf("response_action = %q, want update", resp.ResponseAction)
	}
	if len(resp.View.Blocks) == 0 {
		t.Fatalf("update view has no blocks: %s", rec.Body.String())
	}
	if want := "Saved your `bank`."; resp.View.Blocks[0].Text.Text != want {
		t.Errorf("update view body: got %q, want %q", resp.View.Blocks[0].Text.Text, want)
	}

	pms, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindPayShowSelf})
	if err != nil {
		t.Fatal(err)
	}
	if want := "<@UAAA> :money_with_wings: \n- `bank` - PL61 1090 0000"; pms.Text != want {
		t.Errorf("account status: got %q, want %q", pms.Text, want)
	}
}

func TestViewSubmissionEmptyValueReopensWithErrors(t *testing.T) {
	h, _, _ := newHandler(t)

	payloadJSON := fmt.Sprintf(`{
		"type":"view_submission",
		"user":{"id":"UAAA"},
		"channel":{"id":"C1"},
		"view":{
			"callback_id":%q,
			"state":{"values":{
				%q:{%q:{"selected_option":{"value":"bank"}}},
				%q:{%q:{"value":""}}
			}}
		}
	}`, callbackPayForm, blockMethod, actionMethod, blockValue, actionValue)

	body := url.Values{"payload": []string{payloadJSON}}.Encode()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signForm(t, body, time.Now().Unix()))

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body not JSON: %s", rec.Body.String())
	}
	if resp["response_action"] != "errors" {
		t.Fatalf("expected response_action=errors, got %+v", resp)
	}
	errs, ok := resp["errors"].(map[string]any)
	if !ok || errs[blockValue] == nil {
		t.Fatalf("expected error on %q, got %+v", blockValue, resp)
	}
}

func TestAppMentionPaySetInline(t *testing.T) {
	h, sp, l := newHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, appMention("<@USERGEANT> pay set bank PL61 1090 0000 1234"), time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	p := sp.wait(t)
	if p.ephemeral {
		t.Error("pay set reply should not be ephemeral")
	}
	if want := ":cop: Saved your `bank`."; p.text != want {
		t.Errorf("text: got %q, want %q", p.text, want)
	}
	r, _ := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindPayShowSelf})
	if want := "<@UAAA> :money_with_wings: \n- `bank` - PL61 1090 0000 1234"; r.Text != want {
		t.Errorf("account status: got %q, want %q", r.Text, want)
	}
}

func TestAppMentionPayRemove(t *testing.T) {
	h, sp, l := newHandler(t)
	if _, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindPaySet, PayMethod: "bank", PayValue: "PL61"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, appMention("<@USERGEANT> pay rm bank"), time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	sp.wait(t)
	r, _ := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindPayShowSelf})
	want := "You haven't added any payment methods yet. Try `@sergeant pay set bank PL61 ...` or `@sergeant pay set blik 555 555 555`."
	if r.Text != want {
		t.Errorf("payment methods should be empty: got %q, want %q", r.Text, want)
	}
}

func TestAppMentionPayClear(t *testing.T) {
	h, sp, l := newHandler(t)
	for _, m := range []string{"bank", "blik"} {
		if _, err := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindPaySet, PayMethod: m, PayValue: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, appMention("<@USERGEANT> pay clear"), time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	sp.wait(t)
	r, _ := l.ApplyOnce(context.Background(), "", "UAAA", parser.Command{Kind: parser.KindPayShowSelf})
	want := "You haven't added any payment methods yet. Try `@sergeant pay set bank PL61 ...` or `@sergeant pay set blik 555 555 555`."
	if r.Text != want {
		t.Errorf("expected empty payment list: got %q, want %q", r.Text, want)
	}
}

func TestAppMentionReset(t *testing.T) {
	// Arrange
	h, _, l := newHandler(t)
	ctx := context.Background()
	if _, err := l.ApplyOnce(ctx, "", "UAAA", parser.Command{Kind: parser.KindAdd, Target: "UBBB", Sign: 1, Minor: 2000, Currency: "PLN"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApplyOnce(ctx, "", "UAAA", parser.Command{Kind: parser.KindAdd, Target: "UBBB", Sign: 1, Minor: 1000, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	// Act
	h.ServeHTTP(rec, sign(t, appMention("<@USERGEANT> <@UBBB> reset"), time.Now().Unix()))

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	assertPairStatus(t, l, "UBBB", "<@UBBB> owes you nothing.")
}

func TestAppMentionResetCurrency(t *testing.T) {
	// Arrange
	h, _, l := newHandler(t)
	ctx := context.Background()
	if _, err := l.ApplyOnce(ctx, "", "UAAA", parser.Command{Kind: parser.KindAdd, Target: "UBBB", Sign: 1, Minor: 2000, Currency: "PLN"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ApplyOnce(ctx, "", "UAAA", parser.Command{Kind: parser.KindAdd, Target: "UBBB", Sign: 1, Minor: 1000, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()

	// Act
	h.ServeHTTP(rec, sign(t, appMention("<@USERGEANT> <@UBBB> reset PLN"), time.Now().Unix()))

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	assertPairStatus(t, l, "UBBB", "<@UBBB> owes you 10.00 EUR.")
}

func TestRejectsNonPost(t *testing.T) {
	h, _, _ := newHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/slack/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", rec.Code)
	}
}

func TestRejectsBadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, []byte("not-json"), time.Now().Unix()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestIgnoresUnknownOuterEventType(t *testing.T) {
	h, sp, _ := newHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, []byte(`{"type":"app_rate_limited"}`), time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	select {
	case <-sp.done:
		t.Fatal("responder should not have been called")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServeInteractivityNoPayload(t *testing.T) {
	h, sp, _ := newHandler(t)
	body := url.Values{"foo": []string{"bar"}}.Encode()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signForm(t, body, time.Now().Unix()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	select {
	case <-sp.done:
		t.Fatal("responder should not have been called")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestServeInteractivityBadPayloadJSON(t *testing.T) {
	h, _, _ := newHandler(t)
	body := url.Values{"payload": []string{"not-json"}}.Encode()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signForm(t, body, time.Now().Unix()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestServeInteractivityIgnoresUnknownInteractionType(t *testing.T) {
	h, sp, _ := newHandler(t)
	payload := slack.InteractionCallback{
		Type: slack.InteractionTypeShortcut,
		User: slack.User{ID: "UAAA"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body := url.Values{"payload": []string{string(raw)}}.Encode()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signForm(t, body, time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	select {
	case <-sp.done:
		t.Fatal("responder should not have been called")
	case <-time.After(50 * time.Millisecond):
	}
}

func directMessage(text string) []byte {
	return []byte(fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"UAAA","text":%q,"channel":"D1","ts":"123.456"}}`, text))
}

func directMessageWithBot(text string) []byte {
	return []byte(fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","channel_type":"im","user":"UAAA","bot_id":"B1","text":%q,"channel":"D1","ts":"123.456"}}`, text))
}

func TestDirectMessageQueryDispatch(t *testing.T) {
	h, sp, _ := newHandler(t)

	dispatch(t, h, parseCallbackEvent(t, directMessage("status")))

	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.calls) != 1 || sp.calls[0].ephemeral {
		t.Fatalf("calls = %v, want one non-ephemeral reply", sp.calls)
	}
	if want := ":cop: Nobody owes you and you owe nothing."; sp.calls[0].text != want {
		t.Errorf("text: got %q, want %q", sp.calls[0].text, want)
	}
	if len(sp.reactions) != 1 || sp.reactions[0].name != successEmoji {
		t.Fatalf("reactions = %v, want one %q reaction", sp.reactions, successEmoji)
	}
}

func TestDirectMessageIgnoresBots(t *testing.T) {
	h, sp, _ := newHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, directMessageWithBot("<@UBBB> +20 PLN"), time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	select {
	case <-sp.done:
		t.Fatal("bot DM should be ignored")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDispatchIgnoresUnknownInnerEvent(t *testing.T) {
	h, sp, _ := newHandler(t)
	body := []byte(`{"type":"event_callback","event":{"type":"reaction_added","user":"UAAA","reaction":"thumbsup"}}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, sign(t, body, time.Now().Unix()))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	select {
	case <-sp.done:
		t.Fatal("unknown inner event should not invoke responder")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestStripBotMention(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<@USERGEANT> +1", "+1"},
		{"<@USERGEANT|sergeant> status", "status"},
		{"hello", "hello"},
		{"<@OTHER> +1", "<@OTHER> +1"},
	}
	for _, c := range cases {
		if got := stripBotMention(c.in, "USERGEANT"); got != c.want {
			t.Errorf("strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedeliveredBatchChargesEachLineOnce(t *testing.T) {
	// Arrange
	h, _, l := newHandler(t)
	event := parseCallbackEvent(t, []byte(`{
		"type": "event_callback",
		"event_id": "Ev1",
		"event": {
			"type": "app_mention", "user": "UAAA", "channel": "C1", "ts": "123.456",
			"text": "<@USERGEANT> <@UBBB> +20\n<@UBBB> +20"
		}
	}`))
	dispatch(t, h, event)

	// Act
	dispatch(t, h, event)

	// Assert
	status, err := l.ApplyOnce(t.Context(), "", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: "UBBB"})
	if err != nil || status.Text != "<@UBBB> owes you 40.00 PLN." {
		t.Fatalf("balance after redelivery: %+v, %v", status, err)
	}
}

func TestBatchRetryAfterPartialDatabaseFailure(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "partial.db")
	h, _, accounts := newHandlerAt(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER unavailable BEFORE INSERT ON debts WHEN NEW.debtor_id='UCCC'
		BEGIN SELECT RAISE(ABORT, 'temporary test failure'); END`); err != nil {
		t.Fatal(err)
	}
	body := appMention("<@USERGEANT> <@UBBB> +20\n<@UCCC> +10")
	send := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, sign(t, body, time.Now().Unix()))
		return rec.Code
	}
	if code := send(); code != http.StatusServiceUnavailable {
		t.Fatalf("partial delivery HTTP %d, want 503", code)
	}
	if _, err := db.Exec("DROP TRIGGER unavailable"); err != nil {
		t.Fatal(err)
	}

	// Act
	code := send()

	// Assert
	if code != http.StatusOK {
		t.Fatalf("redelivery HTTP %d", code)
	}
	status, err := accounts.ApplyOnce(t.Context(), "", "UAAA", parser.Command{Kind: parser.KindStatusAll})
	if err != nil {
		t.Fatal(err)
	}
	want := "Owed to you:\n- <@UBBB> - 20.00 PLN\n- <@UCCC> - 10.00 PLN"
	if status.Text != want {
		t.Fatalf("balances after acknowledgement = %q, want %q", status.Text, want)
	}
}

func TestEditedAndAutomatedMentionsLeaveBalancesAlone(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"edited original message", `"edited":{"user":"UAAA","ts":"123.999"}`},
		{"another bot's mention", `"bot_id":"B123"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			h, _, l := newHandler(t)
			body := fmt.Sprintf(`{
				"type": "event_callback",
				"event": {
					"type": "app_mention", "user": "UAAA", "channel": "C1", "ts": "123.456",
					"text": "<@USERGEANT> <@UBBB> +20", %s
				}
			}`, tc.field)
			event := parseCallbackEvent(t, []byte(body))

			// Act
			dispatch(t, h, event)

			// Assert
			got, err := l.ApplyOnce(t.Context(), "", "UAAA", parser.Command{Kind: parser.KindStatusAll})
			if err != nil || got.Text != "Nobody owes you and you owe nothing." {
				t.Fatalf("status: %+v, %v", got, err)
			}
		})
	}
}

func TestPaymentFormKeepsDetailsOpenOnWriteFailure(t *testing.T) {
	// Arrange
	h, _, accounts := newHandler(t)
	if err := accounts.Close(); err != nil {
		t.Fatal(err)
	}
	values := map[string]map[string]slack.BlockAction{
		blockMethod: {actionMethod: {SelectedOption: slack.OptionBlockObject{Value: "bank"}}},
		blockValue:  {actionValue: {Value: "sample details"}},
	}
	cb := slack.InteractionCallback{
		User: slack.User{ID: "UAAA"},
		View: slack.View{State: &slack.ViewState{Values: values}},
	}
	rec := httptest.NewRecorder()

	// Act
	h.handlePayFormSubmission(rec, cb)

	// Assert
	var response struct {
		ResponseAction string `json:"response_action"`
		Errors         map[string]string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ResponseAction != "errors" || response.Errors[blockValue] == "" {
		t.Fatalf("modal response: %+v", response)
	}
}

func TestPaymentFormMissingStateReturnsFieldErrors(t *testing.T) {
	// Arrange
	h, _, _ := newHandler(t)
	rec := httptest.NewRecorder()
	cb := slack.InteractionCallback{User: slack.User{ID: "UAAA"}}

	// Act
	h.handlePayFormSubmission(rec, cb)

	// Assert
	var response struct{ Errors map[string]string }
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Errors[blockMethod] == "" || response.Errors[blockValue] == "" {
		t.Fatalf("missing field errors: %+v", response)
	}
}

func TestMissingPaymentMethodGetsFailureReaction(t *testing.T) {
	// Arrange
	h, sp, _ := newHandler(t)
	event := parseCallbackEvent(t, appMention("<@USERGEANT> pay set-default bank"))

	// Act
	dispatch(t, h, event)

	// Assert
	var verdict []string
	for _, r := range sp.reactions {
		if r.name != mentionEmoji {
			verdict = append(verdict, r.name)
		}
	}
	if !slices.Equal(verdict, []string{failureEmoji}) {
		t.Fatalf("reactions for declined change: %v", sp.reactions)
	}
}

func TestSlackUsesLegacyWhileGhostIsOffline(t *testing.T) {
	// Arrange
	h, sp, accounts := newHandler(t)
	body := appMention("<@USERGEANT> <@UBBB> +20\n<@UCCC> +10")

	// Act
	response := httptest.NewRecorder()
	h.ServeHTTP(response, sign(t, body, time.Now().Unix()))
	h.Wait()

	// Assert
	if response.Code != http.StatusOK || len(sp.reactions) != 2 || len(sp.calls) != 0 {
		t.Fatalf("legacy response HTTP %d, reactions %+v, posts %+v", response.Code, sp.reactions, sp.calls)
	}
	status, err := accounts.ApplyOnce(t.Context(), "query", "UAAA", parser.Command{Kind: parser.KindStatusFor, Target: "UBBB"})
	if err != nil || status.Text != "<@UBBB> owes you 20.00 PLN." {
		t.Fatalf("status %+v, err %v", status, err)
	}
	first, ok, err := accounts.Next(t.Context())
	if err != nil || !ok || first.Command.Target != "UBBB" || first.Command.Minor != 2000 {
		t.Fatalf("queued first line %+v, err %v", first, err)
	}
}
