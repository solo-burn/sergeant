package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"sergeant/internal/engine"
	"sergeant/internal/i18n"
	"sergeant/internal/parser"
)

const (
	maxBodyBytes = 1 << 20
	replyTimeout = 5 * time.Second
	mentionEmoji = "sergeant"
	failureEmoji = "sergeant-no"
	successEmoji = "white_check_mark"
)

type Responder interface {
	PostInThread(ctx context.Context, channel, threadTs, text string) error
	PostEphemeral(ctx context.Context, channel, user, text string) error
	PostEphemeralBlocks(ctx context.Context, channel, user, fallback string, blocks []slack.Block) error
	OpenView(ctx context.Context, triggerID string, view slack.ModalViewRequest) error
	AddReaction(ctx context.Context, channel, ts, name string) error
}

type SlackResponder struct{ Client *slack.Client }

func (r SlackResponder) PostInThread(ctx context.Context, channel, threadTs, text string) error {
	_, _, err := r.Client.PostMessageContext(ctx, channel,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTs),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	return err
}

func (r SlackResponder) PostEphemeral(ctx context.Context, channel, user, text string) error {
	_, err := r.Client.PostEphemeralContext(ctx, channel, user,
		slack.MsgOptionText(text, false),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	return err
}

func (r SlackResponder) PostEphemeralBlocks(
	ctx context.Context, channel, user, fallback string, blocks []slack.Block,
) error {
	_, err := r.Client.PostEphemeralContext(ctx, channel, user,
		slack.MsgOptionText(fallback, false),
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionDisableLinkUnfurl(),
	)
	return err
}

func (r SlackResponder) OpenView(ctx context.Context, triggerID string, view slack.ModalViewRequest) error {
	_, err := r.Client.OpenViewContext(ctx, triggerID, view)
	return err
}

func (r SlackResponder) AddReaction(ctx context.Context, channel, ts, name string) error {
	return r.Client.AddReactionContext(ctx, name, slack.NewRefToMessage(channel, ts))
}

type Handler struct {
	SigningSecret string
	BotUserID     string
	Engine        engine.Engine
	Responder     Responder
	Translator    *i18n.Translator
	Logger        *slog.Logger
	workers       sync.WaitGroup
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Logger.Debug("incoming", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	sv, err := slack.NewSecretsVerifier(r.Header, h.SigningSecret)
	if err == nil {
		_, err = sv.Write(body)
	}
	if err == nil {
		err = sv.Ensure()
	}
	if err != nil {
		h.Logger.Warn("Slack request verification failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		h.serveInteractivity(w, body)
		return
	}

	// The SDK expects event_callback envelopes to contain a non-null event.
	var envelope struct {
		Type  string           `json:"type"`
		Event *json.RawMessage `json:"event"`
	}
	err = json.Unmarshal(body, &envelope)
	if err != nil || (envelope.Type == slackevents.CallbackEvent && envelope.Event == nil) {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	event, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	switch event.Type {
	case slackevents.URLVerification:
		var v slackevents.ChallengeResponse
		if err := json.Unmarshal(body, &v); err != nil {
			http.Error(w, "bad challenge", http.StatusBadRequest)
			return
		}
		h.Logger.Info("url verification challenge")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(v.Challenge))

	case slackevents.CallbackEvent:
		notice, err := h.processEvent(r.Context(), event)
		if err != nil {
			h.Logger.Error("process callback", "err", err)
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		if notice != nil {
			h.background("reply", func() { h.deliver(notice) })
		}

	default:
		h.Logger.Debug("ignoring event", "type", event.Type)
		w.WriteHeader(http.StatusOK)
	}
}

type incoming struct {
	appMention                  bool
	user, channel, ts, threadTs string
	text                        string
}

type feedback struct {
	message     incoming
	acknowledge bool
	openForm    bool
	failed      bool
	text        string
	ephemeral   bool
}

func userMessage(event slackevents.EventsAPIEvent, botUserID string) (incoming, bool) {
	var msg incoming
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if ev.BotID != "" || ev.Edited != nil {
			return incoming{}, false
		}
		msg = incoming{
			appMention: true,
			user:       ev.User,
			channel:    ev.Channel,
			ts:         ev.TimeStamp,
			threadTs:   ev.ThreadTimeStamp,
			text:       ev.Text,
		}
	case *slackevents.MessageEvent:
		if ev.ChannelType != "im" || ev.BotID != "" || ev.SubType != "" {
			return incoming{}, false
		}
		msg = incoming{
			user:     ev.User,
			channel:  ev.Channel,
			ts:       ev.TimeStamp,
			threadTs: ev.ThreadTimeStamp,
			text:     ev.Text,
		}
	default:
		return incoming{}, false
	}
	if msg.user == "" || msg.user == botUserID || msg.ts == "" || msg.channel == "" {
		return incoming{}, false
	}
	if msg.threadTs == "" {
		msg.threadTs = msg.ts
	}
	return msg, true
}

func eventReceiptPrefix(event slackevents.EventsAPIEvent, msg incoming) string {
	if callback, ok := event.Data.(*slackevents.EventsAPICallbackEvent); ok && callback.EventID != "" {
		return "event/" + callback.EventID
	}
	return "msg/" + event.TeamID + "/" + msg.channel + "/" + msg.ts
}

func (h *Handler) processEvent(parent context.Context, event slackevents.EventsAPIEvent) (*feedback, error) {
	msg, ok := userMessage(event, h.BotUserID)
	if !ok {
		return nil, nil
	}
	notice := &feedback{message: msg, acknowledge: msg.appMention}
	commands, err := parser.ParseMessage(stripBotMention(msg.text, h.BotUserID))
	if err != nil {
		notice.failed = true
		notice.text = h.Translator.T(i18n.HandlerUsage)
		notice.ephemeral = true
		return notice, nil
	}

	ctx, cancel := context.WithTimeout(parent, replyTimeout)
	defer cancel()
	receiptPrefix := eventReceiptPrefix(event, msg)
	first := commands[0]
	switch first.Kind {
	case parser.KindAdd, parser.KindReset:
		// ParseMessage permits multiple commands only for debt adjustments.
		err := h.applyDebtChanges(ctx, notice, receiptPrefix, commands)
		return notice, err
	case parser.KindHelp, parser.KindHello:
		notice.text = h.Translator.T(i18n.HandlerUsage)
		notice.ephemeral = first.Kind == parser.KindHelp
		return notice, nil
	case parser.KindPaySetForm:
		notice.openForm = true
		return notice, nil
	}

	reply, err := h.Engine.ApplyOnce(ctx, receiptPrefix+"/0", msg.user, first)
	if err != nil {
		h.Logger.Error("apply account command", "kind", first.Kind, "target", first.Target, "err", err)
		notice.failed = true
		notice.acknowledge = false
		notice.text = h.Translator.T(i18n.HandlerError)
		notice.ephemeral = true
		return notice, err
	}
	notice.failed = reply.Declined
	notice.text = reply.Text
	return notice, nil
}

func (h *Handler) applyDebtChanges(
	ctx context.Context, notice *feedback, receiptPrefix string, commands []parser.Command,
) error {
	var failedUpdates []string
	var retryErr error
	for index, cmd := range commands {
		// Preserve the zero-based position in the receipt even when two lines are identical.
		receiptID := receiptPrefix + "/" + strconv.Itoa(index)
		_, err := h.Engine.ApplyOnce(ctx, receiptID, notice.message.user, cmd)
		if err == nil {
			continue
		}
		h.Logger.Error("apply account command", "kind", cmd.Kind, "target", cmd.Target, "err", err)
		failedUpdates = append(failedUpdates, h.Translator.T(i18n.HandlerFailedUpdate, cmd.Target, index+1))
		if !errors.Is(err, engine.ErrSelfTarget) && !errors.Is(err, engine.ErrBalanceOverflow) {
			retryErr = err
		}
	}

	if len(failedUpdates) == 0 {
		notice.acknowledge = true
		return nil
	}
	notice.acknowledge = false
	notice.failed = true
	if len(commands) > 1 {
		notice.text = h.Translator.T(i18n.HandlerFailedUpdates, strings.Join(failedUpdates, "\n"))
	}
	return retryErr
}

func (h *Handler) deliver(notice *feedback) {
	ctx, cancel := context.WithTimeout(context.Background(), replyTimeout)
	defer cancel()
	react := func(name string) {
		if err := h.Responder.AddReaction(ctx, notice.message.channel, notice.message.ts, name); err != nil {
			h.Logger.Warn("add reaction", "reaction", name, "err", err)
		}
	}
	if notice.acknowledge {
		react(mentionEmoji)
	}
	if notice.failed {
		react(failureEmoji)
	}
	if err := h.postFeedback(ctx, notice); err != nil {
		h.Logger.Error("post reply", "err", err)
		return
	}
	if !notice.failed {
		react(successEmoji)
	}
}

func (h *Handler) postFeedback(ctx context.Context, notice *feedback) error {
	msg := notice.message
	if notice.openForm {
		fallback := h.Translator.T(i18n.PayOpenerText)
		return h.Responder.PostEphemeralBlocks(ctx, msg.channel, msg.user, fallback, payOpenerBlocks(h.Translator))
	}
	if notice.text == "" {
		return nil
	}
	text := ":cop: " + notice.text
	if notice.ephemeral {
		return h.Responder.PostEphemeral(ctx, msg.channel, msg.user, text)
	}
	return h.Responder.PostInThread(ctx, msg.channel, msg.threadTs, text)
}

func (h *Handler) Wait() { h.workers.Wait() }

func (h *Handler) background(label string, fn func()) {
	h.workers.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				h.Logger.Error("panic in goroutine", "where", label, "panic", r)
			}
		}()
		fn()
	})
}

var leadingMentionRE = regexp.MustCompile(`^\s*<@([A-Z0-9]+)(?:\|[^>]*)?>\s*`)

func stripBotMention(text, botID string) string {
	m := leadingMentionRE.FindStringSubmatch(text)
	if m == nil || m[1] != botID {
		return text
	}
	return text[len(m[0]):]
}

func (h *Handler) serveInteractivity(w http.ResponseWriter, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	raw := form.Get("payload")
	if raw == "" {
		http.Error(w, "no payload", http.StatusBadRequest)
		return
	}
	var cb slack.InteractionCallback
	if err := json.Unmarshal([]byte(raw), &cb); err != nil {
		h.Logger.Warn("interaction parse", "err", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	h.Logger.Info("interaction", "type", cb.Type, "user", cb.User.ID, "callback_id", cb.View.CallbackID)

	switch cb.Type {
	case slack.InteractionTypeBlockActions:
		w.WriteHeader(http.StatusOK)
		h.background("handleBlockAction", func() { h.handleBlockAction(cb) })

	case slack.InteractionTypeViewSubmission:
		// view_submission must respond synchronously: empty 200 closes the
		// modal, a JSON `errors` body reopens it with field-level errors.
		if cb.View.CallbackID == callbackPayForm {
			h.handlePayFormSubmission(w, cb)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (h *Handler) handleBlockAction(cb slack.InteractionCallback) {
	ctx, cancel := context.WithTimeout(context.Background(), replyTimeout)
	defer cancel()

	for _, a := range cb.ActionCallback.BlockActions {
		if a != nil && a.ActionID == actionOpenPayForm {
			if err := h.Responder.OpenView(ctx, cb.TriggerID, payModalView(h.Translator)); err != nil {
				h.Logger.Error("open view", "err", err)
			}
			return
		}
	}
}

func (h *Handler) handlePayFormSubmission(w http.ResponseWriter, cb slack.InteractionCallback) {
	if cb.User.ID == "" {
		http.Error(w, "missing user", http.StatusBadRequest)
		return
	}
	var values map[string]map[string]slack.BlockAction
	if cb.View.State != nil {
		values = cb.View.State.Values
	}

	method, validMethod := parser.NormalizeMethod(values[blockMethod][actionMethod].SelectedOption.Value)
	value := strings.TrimSpace(values[blockValue][actionValue].Value)

	fieldErrors := map[string]string{}
	if !validMethod {
		fieldErrors[blockMethod] = h.Translator.T(i18n.PayModalMethod)
	}
	if !parser.ValidPayValue(value) {
		fieldErrors[blockValue] = h.Translator.T(i18n.PayModalValue)
	}
	w.Header().Set("Content-Type", "application/json")
	if len(fieldErrors) != 0 {
		_ = json.NewEncoder(w).Encode(slack.NewErrorsViewSubmissionResponse(fieldErrors))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), replyTimeout)
	defer cancel()
	id := ""
	if cb.View.ID != "" {
		id = "view/" + cb.View.ID
	}
	if _, err := h.Engine.ApplyOnce(ctx, id, cb.User.ID, parser.Command{
		Kind:      parser.KindPaySet,
		PayMethod: method,
		PayValue:  value,
	}); err != nil {
		h.Logger.Error("save payment method", "err", err)
		fieldErrors[blockValue] = h.Translator.T(i18n.HandlerError)
		_ = json.NewEncoder(w).Encode(slack.NewErrorsViewSubmissionResponse(fieldErrors))
		return
	}

	saved := paySavedView(h.Translator, method)
	_ = json.NewEncoder(w).Encode(slack.NewUpdateViewSubmissionResponse(&saved))
}
