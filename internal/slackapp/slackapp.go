// Package slackapp connects noc-bot to Slack as an app over Socket Mode: it posts alerts with
// answer buttons, and passes back what people do (buttons, emoji, thread replies, @mentions).
// Socket Mode dials out to Slack, so noc-bot needs no inbound port.
package slackapp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// Handler is what noc-bot does with people's input. Each call runs in its own goroutine.
type Handler interface {
	// OnAnswer: a button or menu on an alert or digest. value is the option for selects. It returns
	// the reply to post and, when the change can be undone, a key for an undo button.
	OnAnswer(ctx context.Context, incidentID, action, value, user string) (reply, undoKey string)
	// OnUndo: the undo button under a reply.
	OnUndo(ctx context.Context, key, user string) string
	// OnReaction: an emoji added to one of noc-bot's messages.
	OnReaction(ctx context.Context, messageTS, emoji, user string)
	// OnThreadReply: a person wrote in a thread (noc-bot decides whether it is part of it).
	OnThreadReply(ctx context.Context, channel, threadTS, text, user string)
	// OnMention: "@noc-bot ..." anywhere. thread is the thread to answer in.
	OnMention(ctx context.Context, channel, thread, text, user string)
	// OnProposal: a person accepted or rejected something noc-bot proposed to learn.
	OnProposal(ctx context.Context, proposalID string, accept bool, user string) string
}

// Action ids on alert and proposal messages.
const (
	ActNow      = "nb_now"      // 🔴 すぐ知らせて正解
	ActDigest   = "nb_digest"   // 🟡 まとめでいい
	ActMute     = "nb_mute"     // 🔕 通知不要
	ActWrong    = "nb_wrong"    // ❌ 誤検知
	ActSeverity = "nb_severity" // menu: impact 0-3
	ActTeam     = "nb_team"     // menu: owning team
	ActPromote  = "nb_promote"  // digest menu: すぐ知らせて
	ActMuteSel  = "nb_mute_sel" // digest menu: 通知不要
	ActUndoTier = "nb_undo"
	ActAccept   = "nb_accept"
	ActReject   = "nb_reject"
	// kept so buttons on older messages still work
	ActRight   = "nb_right"
	ActSevUp   = "nb_sev_up"
	ActSevDown = "nb_sev_down"
)

// App is the Slack app.
type App struct {
	// Posted, when set, is told the thread root of everything noc-bot posts, so replies there
	// are treated as conversation with it.
	Posted  func(channel, thread string)
	API     *slack.Client
	SM      *socketmode.Client
	Channel string
	Teams   func() []string // team ids for the "担当違う" menu
	Log     *slog.Logger
	h       Handler
	botUser string
}

// New makes an app. botToken is xoxb-..., appToken xapp-... (Socket Mode).
func New(botToken, appToken, channel string, log *slog.Logger) *App {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	return &App{API: api, SM: socketmode.New(api), Channel: channel, Log: log}
}

// Post implements notify.Notifier for plain messages.
func (a *App) Post(ctx context.Context, text, thread string) (string, error) {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false), slack.MsgOptionDisableLinkUnfurl()}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, ts, err := a.API.PostMessageContext(ctx, a.Channel, opts...)
	a.posted(a.Channel, thread, ts, err)
	return ts, err
}

func (a *App) posted(channel, thread, ts string, err error) {
	if err != nil || a.Posted == nil {
		return
	}
	if thread == "" {
		thread = ts
	}
	a.Posted(channel, thread)
}

// PostIncident posts an alert with answer buttons. It returns the message ts (the thread).
func (a *App) PostIncident(ctx context.Context, incidentID, text, thread string) (string, error) {
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, clip(text, 2900), false, false), nil, nil),
		a.answerButtons(incidentID),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType,
			"判定するとAIの学習に使われます。理由やこの件の事情は、このスレッドに書いてください。", false, false)),
	}
	opts := []slack.MsgOption{slack.MsgOptionText(clip(text, 3000), false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionDisableLinkUnfurl()}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, ts, err := a.API.PostMessageContext(ctx, a.Channel, opts...)
	a.posted(a.Channel, thread, ts, err)
	return ts, err
}

func (a *App) answerButtons(incidentID string) *slack.ActionBlock {
	btn := func(id, label string, style slack.Style) *slack.ButtonBlockElement {
		b := slack.NewButtonBlockElement(id, incidentID, slack.NewTextBlockObject(slack.PlainTextType, label, true, false))
		if style != "" {
			b.Style = style
		}
		return b
	}
	elems := []slack.BlockElement{
		btn(ActNow, "🔴 すぐ知らせて正解", slack.StylePrimary),
		btn(ActDigest, "🟡 まとめでいい", ""),
		btn(ActMute, "🔕 通知不要", ""),
		btn(ActWrong, "❌ 誤検知", slack.StyleDanger),
	}
	var sev []*slack.OptionBlockObject
	for i, l := range []string{"0 影響なし", "1 一部の人", "2 多くの人", "3 全体・中核"} {
		sev = append(sev, slack.NewOptionBlockObject(fmt.Sprintf("%s|%d", incidentID, i), slack.NewTextBlockObject(slack.PlainTextType, l, false, false), nil))
	}
	elems = append(elems, slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, slack.NewTextBlockObject(slack.PlainTextType, "影響の大きさ", false, false), ActSeverity, sev...))
	if a.Teams != nil {
		var opts []*slack.OptionBlockObject
		for _, t := range a.Teams() {
			opts = append(opts, slack.NewOptionBlockObject(incidentID+"|"+t, slack.NewTextBlockObject(slack.PlainTextType, t, false, false), nil))
		}
		if len(opts) > 0 {
			elems = append(elems, slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, slack.NewTextBlockObject(slack.PlainTextType, "担当", false, false), ActTeam, opts...))
		}
	}
	return slack.NewActionBlock("nb_answers_"+incidentID, elems...)
}

// DigestItem is one line of the digest's menus.
type DigestItem struct{ ID, Label string }

// PostDigest posts the digest with "すぐ知らせて" and "通知不要" menus over its items.
func (a *App) PostDigest(ctx context.Context, text string, items []DigestItem) (string, error) {
	blocks := []slack.Block{slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, clip(text, 2900), false, false), nil, nil)}
	if len(items) > 0 {
		var up, mute []*slack.OptionBlockObject
		for _, it := range items {
			up = append(up, slack.NewOptionBlockObject(it.ID, slack.NewTextBlockObject(slack.PlainTextType, clip(it.Label, 74), false, false), nil))
			mute = append(mute, slack.NewOptionBlockObject(it.ID, slack.NewTextBlockObject(slack.PlainTextType, clip(it.Label, 74), false, false), nil))
		}
		blocks = append(blocks, slack.NewActionBlock("nb_digest",
			slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, slack.NewTextBlockObject(slack.PlainTextType, "🔴 この種類はすぐ知らせて", false, false), ActPromote, up...),
			slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, slack.NewTextBlockObject(slack.PlainTextType, "🔕 この種類は通知不要", false, false), ActMuteSel, mute...)))
	}
	_, ts, err := a.API.PostMessageContext(ctx, a.Channel, slack.MsgOptionText(clip(text, 3000), false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionDisableLinkUnfurl())
	a.posted(a.Channel, "", ts, err)
	return ts, err
}

// postWithUndo posts a reply with an undo button.
func (a *App) postWithUndo(ctx context.Context, channel, thread, text, key string) error {
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, clip(text, 2900), false, false), nil,
			slack.NewAccessory(slack.NewButtonBlockElement(ActUndoTier, key, slack.NewTextBlockObject(slack.PlainTextType, "取り消す", false, false)))),
	}
	_, ts, err := a.API.PostMessageContext(ctx, channel, slack.MsgOptionText(clip(text, 3000), false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionTS(thread))
	a.posted(channel, thread, ts, err)
	return err
}

// PostProposal posts a "should I learn this?" card in a thread of the alert channel.
func (a *App) PostProposal(ctx context.Context, thread, proposalID, text string) error {
	return a.PostProposalIn(ctx, a.Channel, thread, proposalID, text)
}

// PostProposalIn posts the card in any channel (where a request was made).
func (a *App) PostProposalIn(ctx context.Context, channel, thread, proposalID, text string) error {
	if channel == "" {
		channel = a.Channel
	}
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, clip(text, 2900), false, false), nil, nil),
		slack.NewActionBlock("nb_proposal_"+proposalID,
			withStyle(slack.NewButtonBlockElement(ActAccept, proposalID, slack.NewTextBlockObject(slack.PlainTextType, "登録", false, false)), slack.StylePrimary),
			slack.NewButtonBlockElement(ActReject, proposalID, slack.NewTextBlockObject(slack.PlainTextType, "却下", false, false))),
	}
	_, ts, err := a.API.PostMessageContext(ctx, channel, slack.MsgOptionText(clip(text, 3000), false), slack.MsgOptionBlocks(blocks...), slack.MsgOptionTS(thread))
	a.posted(channel, thread, ts, err)
	return err
}

// PostIn posts plain text to a channel/thread other than the default (answers to mentions).
func (a *App) PostIn(ctx context.Context, channel, thread, text string) error {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false), slack.MsgOptionDisableLinkUnfurl()}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, ts, err := a.API.PostMessageContext(ctx, channel, opts...)
	a.posted(channel, thread, ts, err)
	return err
}

func withStyle(b *slack.ButtonBlockElement, s slack.Style) *slack.ButtonBlockElement {
	b.Style = s
	return b
}

// Run connects and dispatches events until ctx is done.
func (a *App) Run(ctx context.Context, h Handler) error {
	a.h = h
	if r, err := a.API.AuthTestContext(ctx); err != nil {
		return fmt.Errorf("slack auth: %w", err)
	} else {
		a.botUser = r.UserID
		a.Log.Info("slack connected", "team", r.Team, "bot", r.User)
	}
	go func() {
		for evt := range a.SM.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				a.SM.Ack(*evt.Request)
				if ev, ok := evt.Data.(slackevents.EventsAPIEvent); ok {
					go a.onEvent(ctx, ev)
				}
			case socketmode.EventTypeInteractive:
				cb, ok := evt.Data.(slack.InteractionCallback)
				a.SM.Ack(*evt.Request)
				if ok {
					go a.onInteraction(ctx, cb)
				}
			case socketmode.EventTypeConnectionError:
				a.Log.Warn("slack connection error")
			}
		}
	}()
	return a.SM.RunContext(ctx)
}

func (a *App) onEvent(ctx context.Context, ev slackevents.EventsAPIEvent) {
	switch e := ev.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		thread := e.ThreadTimeStamp
		if thread == "" {
			thread = e.TimeStamp
		}
		a.h.OnMention(ctx, e.Channel, thread, e.Text, e.User)
	case *slackevents.MessageEvent:
		// people's replies in threads; ignore bots (including ourselves), edits and mentions
		// (mentions arrive as app_mention too)
		if e.BotID != "" || e.User == "" || e.User == a.botUser || e.SubType != "" || e.ThreadTimeStamp == "" {
			return
		}
		if a.botUser != "" && strings.Contains(e.Text, "<@"+a.botUser+">") {
			return
		}
		a.h.OnThreadReply(ctx, e.Channel, e.ThreadTimeStamp, e.Text, e.User)
	case *slackevents.ReactionAddedEvent:
		if e.User == a.botUser {
			return
		}
		a.h.OnReaction(ctx, e.Item.Timestamp, e.Reaction, e.User)
	}
}

func (a *App) onInteraction(ctx context.Context, cb slack.InteractionCallback) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return
	}
	for _, act := range cb.ActionCallback.BlockActions {
		var reply, undo string
		switch act.ActionID {
		case ActNow, ActDigest, ActMute, ActWrong, ActRight, ActSevUp, ActSevDown:
			reply, undo = a.h.OnAnswer(ctx, act.Value, act.ActionID, "", cb.User.ID)
		case ActTeam, ActSeverity:
			inc, v, _ := strings.Cut(act.SelectedOption.Value, "|")
			reply, undo = a.h.OnAnswer(ctx, inc, act.ActionID, v, cb.User.ID)
		case ActPromote, ActMuteSel:
			reply, undo = a.h.OnAnswer(ctx, act.SelectedOption.Value, act.ActionID, "", cb.User.ID)
		case ActUndoTier:
			reply = a.h.OnUndo(ctx, act.Value, cb.User.ID)
		case ActAccept, ActReject:
			reply = a.h.OnProposal(ctx, act.Value, act.ActionID == ActAccept, cb.User.ID)
		}
		if reply == "" {
			continue
		}
		thread := cb.Message.ThreadTimestamp
		if thread == "" {
			thread = cb.Message.Timestamp
		}
		var err error
		if undo != "" {
			err = a.postWithUndo(ctx, cb.Channel.ID, thread, reply, undo)
		} else {
			err = a.PostIn(ctx, cb.Channel.ID, thread, reply)
		}
		if err != nil {
			a.Log.Warn("slack reply", "err", err)
		}
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Group is a Slack user group.
type Group struct{ ID, Handle, Name string }

// Groups lists the workspace's user groups (needs usergroups:read).
func (a *App) Groups(ctx context.Context) ([]Group, error) {
	gs, err := a.API.GetUserGroupsContext(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(gs))
	for _, g := range gs {
		if g.DateDelete == 0 {
			out = append(out, Group{ID: g.ID, Handle: g.Handle, Name: g.Name})
		}
	}
	return out, nil
}

// Turn is one message of a thread.
type Turn struct {
	User string
	Text string
	Bot  bool // written by noc-bot
}

// Thread returns a thread's messages, oldest first (at most the last 40).
func (a *App) Thread(ctx context.Context, channel, ts string) ([]Turn, error) {
	msgs, _, _, err := a.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{ChannelID: channel, Timestamp: ts, Limit: 200})
	if err != nil {
		return nil, err
	}
	out := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, Turn{User: m.User, Text: m.Text, Bot: m.BotID != "" || (a.botUser != "" && m.User == a.botUser)})
	}
	if len(out) > 40 {
		out = append(out[:1], out[len(out)-39:]...) // keep the opening message for context
	}
	return out, nil
}

// PostLogs puts raw log lines in a thread, folded: as a text snippet (collapsed in Slack) when the
// app has files:write, otherwise as a code block reply.
func (a *App) PostLogs(ctx context.Context, thread, title, logs string) error {
	if strings.TrimSpace(logs) == "" {
		return nil
	}
	_, err := a.API.UploadFileV2Context(ctx, slack.UploadFileV2Parameters{
		Channel: a.Channel, ThreadTimestamp: thread, Filename: "logs.txt", Title: title,
		Content: logs, FileSize: len(logs), SnippetType: "text",
	})
	if err == nil {
		return nil
	}
	a.Log.Info("snippet upload failed, posting logs as a reply (add files:write to the app for snippets)", "err", err)
	_, err = a.Post(ctx, title+"\n```"+clip(logs, 3500)+"```", thread)
	return err
}

// BotUserID is noc-bot's own Slack user id.
func (a *App) BotUserID() string { return a.botUser }

var (
	nameMu sync.Mutex
	names  = map[string]string{}
)

// UserName returns a person's display name (cached; the id itself if it cannot be looked up).
func (a *App) UserName(ctx context.Context, id string) string {
	nameMu.Lock()
	n, ok := names[id]
	nameMu.Unlock()
	if ok {
		return n
	}
	n = id
	if u, err := a.API.GetUserInfoContext(ctx, id); err == nil {
		switch {
		case u.Profile.DisplayName != "":
			n = u.Profile.DisplayName
		case u.RealName != "":
			n = u.RealName
		case u.Name != "":
			n = u.Name
		}
	}
	nameMu.Lock()
	names[id] = n
	nameMu.Unlock()
	return n
}
